package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/psvmcc/roi/internal/buildinfo"
	"github.com/psvmcc/roi/internal/roi"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := newCommand().Run(ctx, os.Args); e != nil {
		slog.Error("roi", "error", e)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name: "roi", Version: buildinfo.Version, Usage: "Single-user RSS/Atom aggregator — ροή",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Value: "config.yaml", Usage: "Path to configuration file", Sources: cli.EnvVars("ROI_CONFIG")},
			&cli.BoolFlag{Name: "check-config", Usage: "Validate configuration and exit", Sources: cli.EnvVars("ROI_CHECK_CONFIG")},
		},
		Commands: []*cli.Command{{Name: "version", Usage: "Show executable version, commit and build date", Action: func(_ context.Context, c *cli.Command) error {
			_, err := fmt.Fprintln(c.Root().Writer, buildinfo.String())
			return err
		}}, {Name: "rotate-api-key", Usage: "Rotate the persisted API key while roi is stopped; invalidates Reader sessions", Action: rotateAPIKey}, {Name: "restore", Usage: "Restore an offline SQLite backup and revoke all restored sessions", Flags: []cli.Flag{&cli.StringFlag{Name: "from", Required: true, Usage: "Backup SQLite file"}}, Action: restoreDatabase}, {Name: "compact", Usage: "Back up and compact SQLite while roi is stopped", Action: compactDatabase}, {Name: "password-hash", Usage: "Generate an Argon2id hash using a hidden terminal prompt", Action: func(_ context.Context, c *cli.Command) error { return passwordHash(c, term.ReadPassword) }}},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 0 {
				return fmt.Errorf("unexpected argument %q", c.Args().First())
			}
			config, e := roi.LoadConfig(c.String("config"))
			if e != nil {
				return e
			}
			if c.Bool("check-config") {
				_, e = fmt.Fprintln(c.Root().Writer, "configuration valid")
				return e
			}
			configureLogging(config.Debug)
			a, e := roi.New(config)
			if e != nil {
				return e
			}
			defer a.Close()
			return a.Run(ctx)
		},
	}
}

func compactDatabase(ctx context.Context, c *cli.Command) error {
	if c.Args().Len() != 0 {
		return fmt.Errorf("compact does not accept arguments")
	}
	config, err := roi.LoadConfig(c.String("config"))
	if err != nil {
		return err
	}
	backup, err := roi.CompactDatabase(ctx, config)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.Root().Writer, "Database compacted. Pre-compaction backup:", backup)
	return err
}

func passwordHash(c *cli.Command, readPassword func(int) ([]byte, error)) error {
	if c.Args().Len() != 0 {
		return fmt.Errorf("password-hash does not accept arguments")
	}
	fmt.Fprint(c.Root().ErrWriter, "Password: ")
	b, e := readPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(c.Root().ErrWriter)
	defer clear(b)
	if e != nil {
		return e
	}
	if len(b) < 12 {
		return fmt.Errorf("password must contain at least 12 bytes")
	}
	h, e := roi.HashPassword(b)
	if e != nil {
		return e
	}
	_, e = fmt.Fprintln(c.Root().Writer, string(h))
	return e
}

func rotateAPIKey(_ context.Context, c *cli.Command) error {
	if c.Args().Len() != 0 {
		return fmt.Errorf("rotate-api-key does not accept arguments")
	}
	config, e := roi.LoadConfig(c.String("config"))
	if e != nil {
		return e
	}
	if e = roi.RotateStoppedAPIKey(config); e != nil {
		return e
	}
	_, e = fmt.Fprintln(c.Root().Writer, "API signing key rotated. Start roi again; all Reader clients must authenticate.")
	return e
}

func restoreDatabase(ctx context.Context, c *cli.Command) error {
	if c.Args().Len() != 0 {
		return fmt.Errorf("restore does not accept positional arguments")
	}
	config, err := roi.LoadConfig(c.String("config"))
	if err != nil {
		return err
	}
	backup, err := roi.RestoreDatabase(ctx, config, c.String("from"))
	if err != nil {
		if backup != "" {
			return fmt.Errorf("restore failed; previous files saved in %s: %w", backup, err)
		}
		return err
	}
	_, err = fmt.Fprintln(c.Root().Writer, "Database restored; all restored browser and Reader sessions revoked. Previous files:", backup)
	return err
}
