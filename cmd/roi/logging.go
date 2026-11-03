package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

// Color is restricted to terminals; redirected logs remain plain text.
type consoleLogWriter struct {
	dst   io.Writer
	color bool
}

func (w consoleLogWriter) Write(p []byte) (int, error) {
	if !w.color || !bytes.Contains(p, []byte(" device=unapproved ")) {
		return w.dst.Write(p)
	}
	line := append([]byte("\x1b[31m"), p...)
	line = append(line, []byte("\x1b[0m")...)
	n, err := w.dst.Write(line)
	if err != nil {
		return 0, err
	}
	if n != len(line) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
func configureLogging(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(consoleLogWriter{dst: os.Stderr, color: term.IsTerminal(int(os.Stderr.Fd()))}, &slog.HandlerOptions{Level: level})))
}
