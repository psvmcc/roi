package roi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type backupRuntime struct {
	Next, Started int64
	Running       bool
}

type backupDay struct {
	Day                                                string
	Attempts, Created, Bytes, Errors                   int64
	LastAttempt, LastFinished, LastSuccess, DurationMS int64
	LastError                                          string
}

type backupFile struct {
	Name            string
	Bytes, Modified int64
}

type backupPanel struct {
	Enabled         bool
	Directory, Time string
	Keep            int
	backupRuntime
	Attempts, Created, CreatedBytes, Errors, StoredCount, StoredBytes int64
	FirstAttempt, LastAttempt, LastSuccess                            int64
	LastError, ScanError                                              string
	Files                                                             []backupFile
	Days                                                              []backupDay
	Page, Pages                                                       int
	PreviousURL, NextURL                                              string
}

func (a *App) dailyBackup(ctx context.Context, now time.Time) (created bool, err error) {
	started := time.Now()
	a.backupMu.Lock()
	a.backupState.Running, a.backupState.Started, a.backupState.Next = true, started.Unix(), 0
	a.backupMu.Unlock()
	defer func() {
		// Persist shutdown failures too; cancellation must not discard accounting.
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		var size int64
		if created {
			info, statErr := os.Lstat(filepath.Join(a.config.backupConfig().Directory, "roi-"+now.UTC().Format("20060102")+".sqlite"))
			if statErr == nil {
				size = info.Size()
			} else {
				err = errors.Join(err, statErr)
			}
		}
		finished := time.Now()
		lastError := ""
		failed, count, success := 0, 0, int64(0)
		if created {
			count = 1
		}
		if err != nil {
			failed, lastError = 1, err.Error()
		} else {
			success = finished.Unix()
		}
		_, recordErr := a.store.DB.ExecContext(recordCtx, `INSERT INTO backup_days(day,attempts,created,bytes,errors,last_attempt,last_finished,last_success,duration_ms,last_error)
 VALUES(?,1,?,?,?,?,?,?,?,?) ON CONFLICT(day) DO UPDATE SET
 attempts=attempts+1,created=created+excluded.created,bytes=bytes+excluded.bytes,errors=errors+excluded.errors,
 last_attempt=excluded.last_attempt,last_finished=excluded.last_finished,
 last_success=MAX(last_success,excluded.last_success),duration_ms=excluded.duration_ms,last_error=excluded.last_error`,
			now.UTC().Format("2006-01-02"), count, size, failed, started.Unix(), finished.Unix(), success, finished.Sub(started).Milliseconds(), lastError)
		if recordErr != nil {
			err = errors.Join(err, fmt.Errorf("record backup statistics: %w", recordErr))
		}
		a.backupMu.Lock()
		a.backupState.Running = false
		a.backupMu.Unlock()
	}()
	return a.createDailyBackup(ctx, now)
}

// Only canonical automatic-backup filenames count toward storage statistics.
func backupFilename(name string) bool {
	if !strings.HasPrefix(name, "roi-") || !strings.HasSuffix(name, ".sqlite") {
		return false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "roi-"), ".sqlite")
	date, err := time.Parse("20060102", stamp)
	return err == nil && date.Format("20060102") == stamp
}

func backupFiles(ctx context.Context, directory, source string) ([]backupFile, int64, error) {
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
		if !backupFilename(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // Retention may remove files during a refresh.
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
