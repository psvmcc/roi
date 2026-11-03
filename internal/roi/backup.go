package roi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

type BackupConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Directory string `yaml:"directory"`
	Time      string `yaml:"time"`
	Keep      int    `yaml:"keep"`
}

func (c Config) backupConfig() BackupConfig {
	b := c.Backup
	if b.Directory == "" {
		b.Directory = filepath.Join(filepath.Dir(c.Database), "backups", "automatic")
	}
	if b.Time == "" {
		b.Time = "03:00"
	}
	if b.Keep == 0 {
		b.Keep = 7
	}
	return b
}
func (b BackupConfig) validate() error {
	at, err := time.Parse("15:04", b.Time)
	if err != nil || at.Format("15:04") != b.Time {
		return errors.New("backup.time must be HH:MM in UTC")
	}
	if b.Keep < 1 {
		return errors.New("backup.keep must be positive")
	}
	if strings.TrimSpace(b.Directory) == "" {
		return errors.New("backup.directory must not be blank")
	}
	return nil
}
func backupTime(now time.Time, at string) time.Time {
	clock, _ := time.Parse("15:04", at)
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
}
func (a *App) runBackups(ctx context.Context) {
	b := a.config.backupConfig()
	if !b.Enabled {
		return
	}
	for ctx.Err() == nil {
		now := time.Now().UTC()
		next := backupTime(now, b.Time)
		if !now.Before(next) {
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			created, err := a.dailyBackup(runCtx, now)
			cancel()
			if created {
				a.metrics.GetOrCreateCounter("roi_backups_total").Inc()
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				a.metrics.GetOrCreateCounter("roi_backup_errors_total").Inc()
				slog.Error("database backup failed", "directory", b.Directory, "error", err)
				next = now.Add(15 * time.Minute)
			} else {
				next = backupTime(now.AddDate(0, 0, 1), b.Time)
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (a *App) dailyBackup(ctx context.Context, now time.Time) (bool, error) {
	b := a.config.backupConfig()
	directory, err := filepath.Abs(b.Directory)
	if err != nil {
		return false, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return false, err
	}
	destination := filepath.Join(directory, "roi-"+now.UTC().Format("20060102")+".sqlite")
	sourceInfo, err := os.Stat(a.config.Database)
	if err != nil {
		return false, err
	}
	created := false
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || os.SameFile(sourceInfo, info) {
			return false, errors.New("backup destination is not an independent regular file")
		}
		if err = checkBackup(ctx, destination); err != nil {
			return false, fmt.Errorf("existing daily backup: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	} else {
		started := time.Now()
		if err = writeBackup(ctx, a.config.Database, directory, destination); err != nil {
			return false, err
		}
		created = true
		a.metrics.GetOrCreateHistogram("roi_backup_duration_seconds").Update(time.Since(started).Seconds())
		slog.Info("database backup created", "path", destination)
	}
	info, err := os.Stat(destination)
	if err != nil {
		return created, err
	}
	a.metrics.GetOrCreateGauge("roi_backup_last_success_timestamp_seconds", nil).Set(float64(info.ModTime().Unix()))
	if err = pruneBackups(directory, b.Keep, sourceInfo); err != nil {
		return created, err
	}
	return created, nil
}
func readOnlyDatabase(path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: absolute}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(100)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func writeBackup(ctx context.Context, source, directory, destination string) (err error) {
	temp, err := os.CreateTemp(directory, ".roi-backup-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(name)
		_ = os.Remove(name + "-wal")
		_ = os.Remove(name + "-shm")
	}()
	if err = temp.Close(); err != nil {
		return err
	}
	db, err := readOnlyDatabase(source)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(raw any) (result error) {
		backup, err := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup(name)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, backup.Finish()) }()
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil {
				var se *sqlite.Error
				if !errors.As(err, &se) || (se.Code()&255 != 5 && se.Code()&255 != 6) {
					return err
				}
			} else if !more {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		}
	})
	if err != nil {
		return err
	}
	if err = checkBackup(ctx, name); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return err
	}
	// Link publishes a complete file atomically without overwriting any existing file.
	if err = os.Link(name, destination); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func checkBackup(ctx context.Context, path string) error {
	db, err := readOnlyDatabase(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("backup integrity check: %s", result)
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='feeds'").Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("backup does not contain an roi database")
	}
	return nil
}
func pruneBackups(directory string, keep int, source os.FileInfo) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "roi-") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "roi-"), ".sqlite")
		date, err := time.Parse("20060102", stamp)
		if err != nil || date.Format("20060102") != stamp {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || os.SameFile(source, info) {
			continue
		}
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i := keep; i < len(names); i++ {
		if err = os.Remove(filepath.Join(directory, names[i])); err != nil {
			return err
		}
	}
	return nil
}
