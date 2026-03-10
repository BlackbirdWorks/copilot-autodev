// Package rotatinglog provides a size-based rotating file writer for slog.
package rotatinglog

import (
	"fmt"
	"os"
	"sync"
)

// Writer is an [io.Writer] that rotates the underlying log file when it
// exceeds maxBytes.  At most maxFiles rotated files are kept; older ones
// are deleted automatically.  Rotation renames in place:
//
//	copilot-autodev.log   → copilot-autodev.log.1
//	copilot-autodev.log.1 → copilot-autodev.log.2
//	…
//
// Writer is safe for concurrent use.
type Writer struct {
	path     string
	maxBytes int64
	maxFiles int

	mu   sync.Mutex
	file *os.File
	size int64
}

// New creates a rotating log writer.  It opens (or creates) the file at path
// and appends to it.  maxSizeMB is the maximum file size in megabytes before
// rotation; maxFiles is the number of rotated files to keep.
func New(path string, maxSizeMB, maxFiles int) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{
		path:     path,
		maxBytes: int64(maxSizeMB) * 1024 * 1024,
		maxFiles: maxFiles,
		file:     f,
		size:     info.Size(),
	}, nil
}

// Write implements [io.Writer].  It rotates the file when the current size
// plus len(p) would exceed maxBytes.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// Best-effort: keep writing to the current file.
			_ = err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// rotate closes the current file, shifts existing rotated files, and opens a
// fresh file.  Must be called with w.mu held.
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}

	// Shift existing rotated files: .3 → delete, .2 → .3, .1 → .2, base → .1
	for i := w.maxFiles; i >= 1; i-- {
		src := w.rotatedName(i - 1)
		dst := w.rotatedName(i)
		if i == w.maxFiles {
			os.Remove(dst) // delete oldest
		}
		_ = os.Rename(src, dst) // ignore errors (file may not exist)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}

// rotatedName returns the file path for the nth rotation.
// n=0 returns the base path; n=1 returns path.1, etc.
func (w *Writer) rotatedName(n int) string {
	if n == 0 {
		return w.path
	}
	return fmt.Sprintf("%s.%d", w.path, n)
}
