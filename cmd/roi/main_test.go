package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psvmcc/roi/internal/roi"
	"go.yaml.in/yaml/v3"
)

func TestConfigEnvironmentAndFlagPrecedence(t *testing.T) {
	example, e := filepath.Abs("../../examples/config.yaml")
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("ROI_CONFIG", example)
	t.Setenv("ROI_CHECK_CONFIG", "true")
	var out bytes.Buffer
	c := newCommand()
	c.Writer = &out
	c.ErrWriter = &out
	if e = c.Run(context.Background(), []string{"roi"}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "configuration valid") {
		t.Fatal(out.String())
	}
	t.Setenv("ROI_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	c = newCommand()
	c.Writer = &out
	c.ErrWriter = &out
	if e = c.Run(context.Background(), []string{"roi", "--config", example, "--check-config"}); e != nil {
		t.Fatal("flag must override ENV:", e)
	}
	c = newCommand()
	c.Writer = &out
	c.ErrWriter = &out
	if e = c.Run(context.Background(), []string{"roi", "-c", example, "--check-config"}); e != nil {
		t.Fatal(e)
	}
}

func TestPasswordHashPromptAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		readErr        error
		wantErr        bool
	}{{"valid", "test-password-123", nil, false}, {"short", "short", nil, true}, {"long", strings.Repeat("a", 73), nil, false}, {"too long", strings.Repeat("a", 4097), nil, true}, {"read error", "partial-secret", io.ErrUnexpectedEOF, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCommand()
			var out, prompt bytes.Buffer
			c.Writer = &out
			c.ErrWriter = &prompt
			if e := c.Run(context.Background(), []string{"roi", "--help"}); e != nil {
				t.Fatal(e)
			}
			out.Reset()
			prompt.Reset()
			secret := []byte(tc.password)
			e := passwordHash(c, func(int) ([]byte, error) { return secret, tc.readErr })
			if (e != nil) != tc.wantErr {
				t.Fatal(e)
			}
			if strings.Contains(prompt.String(), tc.password) || strings.Contains(out.String(), tc.password) {
				t.Fatal("password echoed")
			}
			for _, b := range secret {
				if b != 0 {
					t.Fatal("password buffer not cleared")
				}
			}
			if !tc.wantErr {
				if !roi.VerifyPassword(strings.TrimSpace(out.String()), []byte(tc.password)) {
					t.Fatal("hash verification failed")
				}
			}
		})
	}

}

func TestCLIHelpErrorsAndCancelledRun(t *testing.T) {
	t.Setenv("ROI_CHECK_CONFIG", "false")
	t.Setenv("ROI_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, args := range [][]string{{"roi", "--help"}, {"roi", "password-hash", "--help"}} {
		c := newCommand()
		var out bytes.Buffer
		c.Writer = &out
		c.ErrWriter = &out
		if e := c.Run(context.Background(), args); e != nil || out.Len() == 0 {
			t.Fatal(e, out.String())
		}
	}
	for _, args := range [][]string{{"roi"}, {"roi", "unexpected"}, {"roi", "password-hash", "unexpected"}, {"roi", "--unknown"}} {
		c := newCommand()
		c.Writer = io.Discard
		c.ErrWriter = io.Discard
		if e := c.Run(context.Background(), args); e == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	raw, e := os.ReadFile("../../examples/config.yaml")
	if e != nil {
		t.Fatal(e)
	}
	var config roi.Config
	if e = yaml.Unmarshal(raw, &config); e != nil {
		t.Fatal(e)
	}
	listeners := []net.Listener{}
	for i := range 3 {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		listeners = append(listeners, l)
		switch i {
		case 0:
			config.API.Listen = l.Addr().String()
		case 1:
			config.Admin.Listen = l.Addr().String()
		case 2:
			config.Ops.Listen = l.Addr().String()
		}
	}
	for _, l := range listeners {
		l.Close()
	}
	config.Database = filepath.Join(t.TempDir(), "roi.db")
	file := filepath.Join(t.TempDir(), "config.yaml")
	write := func() {
		t.Helper()
		b, e := yaml.Marshal(config)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(file, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	t.Setenv("ROI_CONFIG", file)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCommand()
	c.Writer = io.Discard
	c.ErrWriter = io.Discard
	if e = c.Run(ctx, []string{"roi"}); e != nil {
		t.Fatal(e)
	}
	config.API.Listen = "invalid"
	write()
	c = newCommand()
	c.Writer = io.Discard
	c.ErrWriter = io.Discard
	if e = c.Run(ctx, []string{"roi"}); e == nil {
		t.Fatal("invalid configuration accepted")
	}
	config.API.Listen = "127.0.0.1:0"
	config.Database = filepath.Join(file, "not-a-directory")
	write()
	c = newCommand()
	c.Writer = io.Discard
	c.ErrWriter = io.Discard
	if e = c.Run(ctx, []string{"roi"}); e == nil {
		t.Fatal("database open failure ignored")
	}
}

func TestRotateAPIKeyCommand(t *testing.T) {
	config, e := roi.LoadConfig("../../examples/config.yaml")
	if e != nil {
		t.Fatal(e)
	}
	config.Database = filepath.Join(t.TempDir(), "roi.db")
	file := filepath.Join(t.TempDir(), "config.yaml")
	b, e := yaml.Marshal(config)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	store, e := roi.OpenStore(config.Database)
	if e != nil {
		t.Fatal(e)
	}
	before, e := store.SigningKey()
	store.DB.Close()
	if e != nil {
		t.Fatal(e)
	}
	c := newCommand()
	var out bytes.Buffer
	c.Writer = &out
	c.ErrWriter = &out
	if e = c.Run(context.Background(), []string{"roi", "--config", file, "rotate-api-key"}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "rotated") {
		t.Fatal(out.String())
	}
	store, e = roi.OpenStore(config.Database)
	if e != nil {
		t.Fatal(e)
	}
	after, e := store.SigningKey()
	store.DB.Close()
	if e != nil || before == after || strings.Contains(out.String(), after) {
		t.Fatal("rotation or output confidentiality failed", e)
	}
	for _, args := range [][]string{{"roi", "--config", file, "rotate-api-key", "unexpected"}, {"roi", "--config", file + ".missing", "rotate-api-key"}} {
		c := newCommand()
		c.Writer = io.Discard
		c.ErrWriter = io.Discard
		if e := c.Run(context.Background(), args); e == nil {
			t.Fatal("invalid rotation command accepted")
		}
	}
	config.Database = filepath.Join(file, "invalid.db")
	b, e = yaml.Marshal(config)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	c = newCommand()
	c.Writer = io.Discard
	c.ErrWriter = io.Discard
	if e = c.Run(context.Background(), []string{"roi", "--config", file, "rotate-api-key"}); e == nil {
		t.Fatal("storage error ignored")
	}
}

func TestVersionCommandDoesNotNeedConfiguration(t *testing.T) {
	var out bytes.Buffer
	command := newCommand()
	command.Writer = &out
	if err := command.Run(t.Context(), []string{"roi", "--config", "does-not-exist.yaml", "version"}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"roi ", "commit:", "built:", "go:"} {
		if !strings.Contains(out.String(), part) {
			t.Fatal(out.String())
		}
	}
}

func TestCompactDatabaseCommand(t *testing.T) {
	config, err := roi.LoadConfig("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config.Database = filepath.Join(t.TempDir(), "roi.sqlite")
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := newCommand()
	var output bytes.Buffer
	command.Writer = &output
	command.ErrWriter = &output
	if err = command.Run(context.Background(), []string{"roi", "--config", file, "compact"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Pre-compaction backup:") {
		t.Fatal(output.String())
	}
	command = newCommand()
	command.Writer = io.Discard
	command.ErrWriter = io.Discard
	if err = command.Run(context.Background(), []string{"roi", "--config", file, "compact", "unexpected"}); err == nil {
		t.Fatal("arguments accepted")
	}
	command = newCommand()
	command.Writer = io.Discard
	command.ErrWriter = io.Discard
	if err = command.Run(context.Background(), []string{"roi", "--config", file + "missing", "compact"}); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestSafeRestoreCommand(t *testing.T) {
	config, err := roi.LoadConfig("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config.Database = filepath.Join(t.TempDir(), "destination.sqlite")
	source := filepath.Join(t.TempDir(), "source.sqlite")
	store, err := roi.OpenStore(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	for index, args := range [][]string{{"roi", "--config", file, "restore", "--from", source}, {"roi", "--config", file, "restore", "--from", source, "extra"}, {"roi", "--config", file + "missing", "restore", "--from", source}, {"roi", "--config", file, "restore", "--from", source + "missing"}} {
		command := newCommand()
		var output bytes.Buffer
		command.Writer = &output
		command.ErrWriter = &output
		err = command.Run(context.Background(), args)
		if index == 0 {
			if err != nil || !strings.Contains(output.String(), "all restored browser and Reader sessions revoked") {
				t.Fatal(err, output.String())
			}
		} else if err == nil {
			t.Fatal("invalid restore command accepted", args)
		}
	}
}
