package roi

import (
	"net/http"
	"path/filepath"
	"time"
)

type subscriptionBackupPanel struct {
	Enabled         bool
	Directory, Time string
	Keep            int
	backupRuntime
	subscriptionBackupState
	StoredCount, StoredBytes int64
	Files                    []backupFile
	ScanError                string
}

func (a *App) subscriptionBackupStatistics(r *http.Request, panel *subscriptionBackupPanel) error {
	b := a.config.subscriptionBackupConfig()
	panel.Enabled, panel.Directory, panel.Time, panel.Keep = b.Enabled, b.Directory, b.Time, b.Keep
	if !b.Enabled {
		return nil
	}
	directory, err := filepath.Abs(b.Directory)
	if err != nil {
		return err
	}
	panel.subscriptionBackupState, err = a.subscriptionBackupState(r.Context(), directory)
	if err != nil {
		return err
	}
	panel.Files, panel.StoredBytes, err = subscriptionFiles(r.Context(), directory, a.config.Database)
	if err != nil {
		panel.ScanError = err.Error()
	}
	panel.StoredCount = int64(len(panel.Files))
	if len(panel.Files) > 50 {
		panel.Files = panel.Files[:50]
	}
	a.backupMu.Lock()
	panel.backupRuntime = a.subscriptionState
	a.backupMu.Unlock()
	if panel.Next == 0 && !panel.Running {
		now := time.Now().UTC()
		next := backupTime(now, b.Time)
		if panel.CheckedDay == now.Format("2006-01-02") {
			next = backupTime(now.AddDate(0, 0, 1), b.Time)
		} else if panel.StoredCount == 0 || !now.Before(next) {
			next = now
		}
		if panel.Error != "" && panel.LastChecked+900 > next.Unix() {
			next = time.Unix(panel.LastChecked+900, 0)
		}
		panel.Next = next.Unix()
	}
	return nil
}
