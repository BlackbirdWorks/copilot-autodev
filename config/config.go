// Package config provides configuration loading from config.yaml.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
type Config struct {
	GitHubOwner         string `yaml:"github_owner"`
	GitHubRepo          string `yaml:"github_repo"`
	MaxConcurrentIssues int    `yaml:"max_concurrent_issues"`
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
	// RefinementPrompt is the instructional text sent to @copilot after CI
	// passes.  The orchestrator prefixes it with the refinement-check counter
	// and appends the machine-readable marker; users only need to supply the
	// "what to do" text.  Defaults to a sensible built-in prompt.
	RefinementPrompt string `yaml:"refinement_prompt"`
}

// Load reads config.yaml from the given path and returns a populated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	cfg := &Config{
		MaxConcurrentIssues: 3,
		PollIntervalSeconds: 45,
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	if cfg.GitHubOwner == "" {
		return nil, fmt.Errorf("config: github_owner is required")
	}
	if cfg.GitHubRepo == "" {
		return nil, fmt.Errorf("config: github_repo is required")
	}
	if cfg.MaxConcurrentIssues < 1 {
		cfg.MaxConcurrentIssues = 1
	}
	if cfg.PollIntervalSeconds < 10 {
		cfg.PollIntervalSeconds = 10
	}

	return cfg, nil
}
