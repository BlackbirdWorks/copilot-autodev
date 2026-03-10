// copilot-autocode is a terminal UI application that acts as a headless
// "Copilot Orchestrator".  It manages a queue of GitHub issues, feeds them to
// the native GitHub Copilot coding agent, and babysits the resulting pull
// requests through CI feedback and merging.
//
// Usage:
//
//	GITHUB_TOKEN=<pat> copilot-autocode [--config config.yaml]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gen2brain/beeep"

	"github.com/BlackbirdWorks/copilot-autocode/config"
	"github.com/BlackbirdWorks/copilot-autocode/ghclient"
	"github.com/BlackbirdWorks/copilot-autocode/pkgs/logger"
	"github.com/BlackbirdWorks/copilot-autocode/pkgs/rotatinglog"
	"github.com/BlackbirdWorks/copilot-autocode/poller"
	"github.com/BlackbirdWorks/copilot-autocode/tui"
)

const (
	logBufferSize = 100 // size of the channel between logWriter and TUI
)

// logWriter intercepts [log.Printf] output and forwards it to Bubble Tea as a LogEvent.
type logWriter struct {
	prog  *tea.Program
	msgCh chan string
}

func (w *logWriter) start() {
	go func() {
		for msg := range w.msgCh {
			if w.prog != nil {
				w.prog.Send(tui.LogEvent{Message: msg})
			}
		}
	}()
}

func (w *logWriter) Write(p []byte) (int, error) {
	// slog.TextHandler adds a trailing newline; strip it for the TUI LogEvent.
	text := string(p)
	if len(text) > 0 && text[len(text)-1] == '\n' {
		text = text[:len(text)-1]
	}
	select {
	case w.msgCh <- text:
	default:
		// Drop if buffer full to avoid blocking main thread.
	}
	return len(p), nil
}

func main() {
	bootstrapLogger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		bootstrapLogger.Error("GITHUB_TOKEN environment variable is required")
		os.Exit(1)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		bootstrapLogger.Error("failed to load config", slog.Any("err", err))
		os.Exit(1)
	}

	gh := ghclient.New(token, cfg)

	// Create and start the background poller.
	p := poller.New(cfg, gh, token)

	// Create the Bubble Tea model with the poller's command channel.
	model := tui.New(cfg.GitHubOwner, cfg.GitHubRepo, cfg.PollIntervalSeconds, p.Commands, gh)

	prog := tea.NewProgram(
		model,
		tea.WithAltScreen(),
	)

	// Configure slog to forward to both the TUI and a local log file.
	lw := &logWriter{
		prog:  prog,
		msgCh: make(chan string, logBufferSize),
	}
	lw.start()

	var logDest io.Writer = lw
	rl, err := rotatinglog.New("copilot-autocode.log", cfg.LogMaxSizeMB, cfg.LogMaxFiles)
	if err != nil {
		bootstrapLogger.Warn("could not open log file; logging to TUI only", slog.Any("err", err))
	} else {
		defer rl.Close()
		logDest = io.MultiWriter(lw, rl)
	}

	handler := slog.NewTextHandler(logDest, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	// Redirect standard log.Printf calls (e.g. from dependencies) to slog.
	log.SetOutput(slog.NewLogLogger(handler, slog.LevelInfo).Writer())

	ctx, cancel := context.WithCancel(context.Background())
	ctx = logger.Save(ctx, slog.Default())

	logger.Load(ctx).InfoContext(ctx, "Copilot Orchestrator starting...")

	p.Start(ctx)

	// Bridge poller events → Bubble Tea messages in a goroutine.
	// Also detect state transitions for desktop notifications.
	go func() {
		prevReview := make(map[int]string) // issue number → current status
		for evt := range p.Events {
			prog.Send(tui.PollEvent{Event: evt})

			if !cfg.NotificationsEnabled {
				continue
			}

			// Build current review map.
			currReview := make(map[int]string, len(evt.Review))
			for _, s := range evt.Review {
				num := s.Issue.GetNumber()
				currReview[num] = s.CurrentStatus
			}

			// Detect PRs that disappeared from review (merged).
			for num := range prevReview {
				if _, still := currReview[num]; !still {
					_ = beeep.Notify(
						"PR Merged",
						fmt.Sprintf("Issue #%d completed and merged", num),
						"",
					)
				}
			}

			// Detect new problematic states.
			for num, status := range currReview {
				prev := prevReview[num]
				if status == prev {
					continue
				}
				if strings.Contains(status, "needs manual fix") {
					_ = beeep.Notify(
						"Manual Fix Needed",
						fmt.Sprintf("Issue #%d: merge conflicts need manual resolution", num),
						"",
					)
				} else if strings.Contains(status, "unresponsive") {
					_ = beeep.Notify(
						"Agent Timeout",
						fmt.Sprintf("Issue #%d: agent unresponsive after retries", num),
						"",
					)
				}
			}

			prevReview = currReview
		}
	}()

	// Handle OS signals for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		prog.Quit()
	}()

	if _, err := prog.Run(); err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error running TUI: %v\n", err)
		os.Exit(1)
	}
	cancel()
}
