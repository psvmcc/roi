package roi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type subscriptionBackupState struct {
	CheckedDay, CreatedDay                   string
	LastChecked, LastCreated, Created, Bytes int64
	Result, Error                            string
}

func (c Config) subscriptionBackupConfig() BackupConfig {
	b := c.SubscriptionBackup
	if b.Directory == "" {
		b.Directory = filepath.Join(filepath.Dir(c.Database), "backups", "subscriptions")
	}
	if b.Time == "" {
		b.Time = "03:00"
	}
	if b.Keep == 0 {
		b.Keep = 30
	}
	return b
}
func (a *App) subscriptionBackupState(ctx context.Context, directory string) (subscriptionBackupState, error) {
	var state subscriptionBackupState
	err := a.store.DB.QueryRowContext(ctx, `SELECT checked_day,created_day,last_checked,last_created,created,bytes,result,error FROM subscription_backup_state WHERE directory=?`, directory).Scan(&state.CheckedDay, &state.CreatedDay, &state.LastChecked, &state.LastCreated, &state.Created, &state.Bytes, &state.Result, &state.Error)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return state, err
}
func subscriptionFilename(name string) bool {
	if !strings.HasPrefix(name, "roi-") || !strings.HasSuffix(name, ".roi") {
		return false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "roi-"), ".roi")
	day, err := time.Parse("20060102", stamp)
	return err == nil && day.Format("20060102") == stamp
}
func subscriptionFiles(ctx context.Context, directory, source string) ([]backupFile, int64, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return nil, 0, err
	}
	files := []backupFile{}
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !subscriptionFilename(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() || os.SameFile(sourceInfo, info) {
			continue
		}
		files = append(files, backupFile{Name: entry.Name(), Bytes: info.Size(), Modified: info.ModTime().Unix()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files, total, nil
}
func readSubscriptionBackup(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("ROI backup is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("ROI backup changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxROIBytes+1))
	if err != nil {
		return nil, err
	}
	if _, err = decodeSubscriptions(bytes.NewReader(data), "roi"); err != nil {
		return nil, fmt.Errorf("invalid ROI backup: %w", err)
	}
	return data, nil
}
func publishSubscriptionBackup(ctx context.Context, directory, destination string, data []byte) (created bool, err error) {
	if err = ctx.Err(); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(directory, ".roi-subscriptions-*.tmp")
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		return false, err
	}
	if err = file.Sync(); err != nil {
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if err = os.Link(file.Name(), destination); err != nil {
		return false, err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return true, err
	}
	return true, errors.Join(dir.Sync(), dir.Close())
}
func pruneSubscriptionBackups(ctx context.Context, directory string, keep int, source string) error {
	files, _, err := subscriptionFiles(ctx, directory, source)
	if err != nil {
		return err
	}
	for _, file := range files[min(keep, len(files)):] {
		if err = os.Remove(filepath.Join(directory, file.Name)); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) checkSubscriptionBackup(ctx context.Context, now time.Time) (created bool, err error) {
	a.subscriptionRunMu.Lock()
	defer a.subscriptionRunMu.Unlock()
	b := a.config.subscriptionBackupConfig()
	directory, err := filepath.Abs(b.Directory)
	if err != nil {
		return false, err
	}
	state, err := a.subscriptionBackupState(ctx, directory)
	if err != nil {
		return false, err
	}
	day := now.UTC().Format("2006-01-02")
	if state.CheckedDay == day && state.Error == "" {
		return false, nil
	}
	files, _, err := subscriptionFiles(ctx, directory, a.config.Database)
	if err != nil {
		return false, a.subscriptionBackupFailure(ctx, directory, now, err)
	}
	// The first snapshot is immediate; later comparisons run at the configured daily time.
	if len(files) > 0 && now.Before(backupTime(now, b.Time)) {
		return false, nil
	}
	a.backupMu.Lock()
	a.subscriptionState = backupRuntime{Started: time.Now().Unix(), Running: true}
	a.backupMu.Unlock()
	var size int64
	result := "Unchanged"
	defer func() {
		if created {
			state.Created++
			state.Bytes += size
			state.LastCreated = now.Unix()
			state.CreatedDay = day
		}
		state.LastChecked = now.Unix()
		state.Result = result
		state.Error = ""
		if err == nil {
			state.CheckedDay = day
		} else {
			state.Error = err.Error()
			state.Result = "Failed"
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, recordErr := a.store.DB.ExecContext(recordCtx, `INSERT INTO subscription_backup_state(directory,checked_day,created_day,last_checked,last_created,created,bytes,result,error) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(directory) DO UPDATE SET checked_day=excluded.checked_day,created_day=excluded.created_day,last_checked=excluded.last_checked,last_created=excluded.last_created,created=excluded.created,bytes=excluded.bytes,result=excluded.result,error=excluded.error`, directory, state.CheckedDay, state.CreatedDay, state.LastChecked, state.LastCreated, state.Created, state.Bytes, state.Result, state.Error)
		if recordErr != nil {
			err = errors.Join(err, fmt.Errorf("record subscription backup status: %w", recordErr))
		}
		a.backupMu.Lock()
		a.subscriptionState.Running = false
		a.backupMu.Unlock()
	}()
	destination := filepath.Join(directory, "roi-"+now.UTC().Format("20060102")+".roi")
	if _, statErr := os.Lstat(destination); statErr == nil {
		if _, err = readSubscriptionBackup(destination); err != nil {
			return false, err
		}
		result = "Today's copy already exists"
		return false, pruneSubscriptionBackups(ctx, directory, b.Keep, a.config.Database)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if state.CreatedDay == day {
		return false, errors.New("today's ROI backup is missing; it will not be recreated on the same day")
	}
	var data bytes.Buffer
	if err = a.store.exportROI(ctx, &data); err != nil {
		return false, err
	}
	if _, err = decodeSubscriptions(bytes.NewReader(data.Bytes()), "roi"); err != nil {
		return false, fmt.Errorf("ROI export cannot be restored: %w", err)
	}
	if len(files) > 0 {
		previous, readErr := readSubscriptionBackup(filepath.Join(directory, files[0].Name))
		if readErr != nil {
			return false, readErr
		}
		if sha256.Sum256(previous) == sha256.Sum256(data.Bytes()) {
			return false, pruneSubscriptionBackups(ctx, directory, b.Keep, a.config.Database)
		}
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return false, err
	}
	size = int64(data.Len())
	created, err = publishSubscriptionBackup(ctx, directory, destination, data.Bytes())
	if err != nil {
		return created, err
	}
	result = "Created a new copy"
	slog.Info("subscription backup created", "path", destination)
	return true, pruneSubscriptionBackups(ctx, directory, b.Keep, a.config.Database)
}
func (a *App) subscriptionBackupFailure(ctx context.Context, directory string, now time.Time, cause error) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err := a.store.DB.ExecContext(recordCtx, `INSERT INTO subscription_backup_state(directory,last_checked,result,error) VALUES(?,?,'Failed',?) ON CONFLICT(directory) DO UPDATE SET last_checked=excluded.last_checked,result=excluded.result,error=excluded.error`, directory, now.Unix(), cause.Error())
	return errors.Join(cause, err)
}
func (a *App) runSubscriptionBackups(ctx context.Context) {
	b := a.config.subscriptionBackupConfig()
	if !b.Enabled {
		return
	}
	for ctx.Err() == nil {
		now := time.Now().UTC()
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err := a.checkSubscriptionBackup(runCtx, now)
		cancel()
		if ctx.Err() != nil {
			return
		}
		next := backupTime(now, b.Time)
		if !now.Before(next) {
			next = backupTime(now.AddDate(0, 0, 1), b.Time)
		}
		if err != nil {
			slog.Error("subscription backup failed", "directory", b.Directory, "error", err)
			next = time.Now().Add(15 * time.Minute)
		}
		a.backupMu.Lock()
		a.subscriptionState.Next = next.Unix()
		a.backupMu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
