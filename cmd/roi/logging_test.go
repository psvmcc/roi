package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type brokenLogWriter struct{}

func (brokenLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestConsoleUnapprovedColorAndPlainLogs(t *testing.T) {
	for _, color := range []bool{true, false} {
		var out bytes.Buffer
		w := consoleLogWriter{dst: &out, color: color}
		line := []byte("level=DEBUG msg=request device=unapproved device_state=unapproved\n")
		n, err := w.Write(line)
		if err != nil || n != len(line) || bytes.Contains(out.Bytes(), []byte("\x1b[31m")) != color {
			t.Fatal(out.String(), n, err)
		}
	}
	var out bytes.Buffer
	_, _ = (consoleLogWriter{dst: &out, color: true}).Write([]byte("device=Mac\n"))
	if bytes.Contains(out.Bytes(), []byte("\x1b")) {
		t.Fatal("approved device colored")
	}
	if _, err := (consoleLogWriter{dst: brokenLogWriter{}, color: true}).Write([]byte(" device=unapproved x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	previous := slog.Default()
	defer slog.SetDefault(previous)
	configureLogging(false)
	configureLogging(true)
}
