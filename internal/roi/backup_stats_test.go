package roi

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func backupView(t *testing.T, a *App, query string) backupPanel {
	t.Helper()
	var panel backupPanel
	if err := a.backupStatistics(httptest.NewRequest("GET", "/maintenance"+query, nil), &panel); err != nil {
		t.Fatal(err)
	}
	return panel
}

func TestBackupStatisticsRetentionRestartAndDailyTotals(t *testing.T) {
	a := testApp(t)
	a.config.Backup = BackupConfig{Enabled: true, Directory: t.TempDir(), Keep: 2, Time: "03:00"}
	now := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	for i := range 3 {
		if created, err := a.dailyBackup(t.Context(), now.AddDate(0, 0, i)); err != nil || !created {
			t.Fatal(created, err)
		}
	}
	first := backupView(t, a, "")
	if first.Created != 3 || first.Attempts != 3 || first.StoredCount != 2 || first.Errors != 0 || len(first.Days) != 3 || first.CreatedBytes <= first.StoredBytes || first.FirstAttempt == 0 || first.LastSuccess == 0 {
		t.Fatalf("%+v", first)
	}
	if created, err := a.dailyBackup(t.Context(), now.AddDate(0, 0, 2)); err != nil || created {
		t.Fatal(created, err)
	}
	after := backupView(t, a, "")
	if after.Created != 3 || after.Attempts != 4 || after.CreatedBytes != first.CreatedBytes || after.Days[0].Attempts != 2 || after.Days[0].Created != 1 || after.Days[0].Day != "2026.11.04" {
		t.Fatalf("%+v", after)
	}
	cookie, _ := adminSession(t, a)
	page := adminRequest(a, cookie, "GET", "/maintenance", nil)
	for _, want := range []string{`id="backup-statistics"`, `aria-label="Daily backup history"`, `aria-label="Stored backups"`, "3 copies", "roi-20261104.sqlite", "2026.11.04", "Task runs / errors", "Copies created (recorded total)", "Next run", "A restart checks today's existing backup; it does not create or overwrite it."} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), want) {
			t.Fatal(want, page.Code)
		}
	}
	// Files alone cannot reconstruct lifetime totals after retention; SQLite retains them.
	c := a.config
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted := backupView(t, reopened, "")
	if persisted.Created != 3 || persisted.Attempts != 4 || persisted.CreatedBytes != after.CreatedBytes || persisted.StoredCount != 2 {
		t.Fatalf("%+v", persisted)
	}
}

func TestBackupStatisticsFailuresFilesAndDisabled(t *testing.T) {
	a := testApp(t)
	if panel := backupView(t, a, ""); panel.Enabled {
		t.Fatal("disabled statistics visible")
	}
	a.config.Backup = BackupConfig{Enabled: true, Directory: t.TempDir(), Keep: 2, Time: "03:00"}
	now := time.Now().UTC()
	path := filepath.Join(a.config.Backup.Directory, "roi-"+now.Format("20060102")+".sqlite")
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dailyBackup(t.Context(), now); err == nil {
		t.Fatal("corrupt copy accepted")
	}
	panel := backupView(t, a, "")
	if panel.Errors != 1 || panel.Created != 0 || panel.LastError == "" || panel.Attempts != 1 || panel.LastSuccess != 0 {
		t.Fatalf("%+v", panel)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dailyBackup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	panel = backupView(t, a, "")
	if panel.Errors != 1 || panel.Created != 1 || panel.LastError != "" || panel.LastSuccess == 0 {
		t.Fatalf("%+v", panel)
	}
	for _, name := range []string{"manual.sqlite", "roi-20261301.sqlite", ".roi-backup-partial.tmp"} {
		if err := os.WriteFile(filepath.Join(a.config.Backup.Directory, name), []byte("ignore"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(path, filepath.Join(a.config.Backup.Directory, "roi-20000101.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a.config.Database, filepath.Join(a.config.Backup.Directory, "roi-20000102.sqlite")); err != nil {
		t.Fatal(err)
	}
	files, bytes, err := backupFiles(t.Context(), a.config.Backup.Directory, a.config.Database)
	if err != nil || len(files) != 1 || bytes != panel.StoredBytes {
		t.Fatal(files, bytes, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := backupFiles(canceled, a.config.Backup.Directory, a.config.Database); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := backupFiles(t.Context(), a.config.Backup.Directory, "missing"); err == nil {
		t.Fatal("source error ignored")
	}
	a.config.Backup.Directory = path
	if panel := backupView(t, a, ""); panel.ScanError == "" {
		t.Fatal("directory failure hidden")
	}
	a.config.Backup.Directory = filepath.Join(t.TempDir(), "missing")
	if panel := backupView(t, a, ""); panel.StoredCount != 0 || panel.ScanError != "" {
		t.Fatalf("%+v", panel)
	}
}

func TestBackupHistoryPaginationAndRuntime(t *testing.T) {
	a := testApp(t)
	a.config.Backup = BackupConfig{Enabled: true, Directory: t.TempDir(), Keep: 2, Time: "00:00"}
	for i := range 35 {
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
		mustExec(t, a, "INSERT INTO backup_days(day,attempts,created,bytes,last_attempt,last_finished) VALUES(?,1,1,100,100,101)", day)
	}
	for _, tc := range []struct {
		q          string
		page, rows int
	}{{"", 1, 30}, {"?backup_page=2", 2, 5}, {"?backup_page=999", 2, 5}, {"?backup_page=bad", 1, 30}} {
		panel := backupView(t, a, tc.q)
		if panel.Page != tc.page || panel.Pages != 2 || len(panel.Days) != tc.rows || panel.Created != 35 || panel.CreatedBytes != 3500 {
			t.Fatalf("%+v", panel)
		}
		if tc.page == 1 && !strings.Contains(panel.NextURL, "/maintenance?backup_page=2") {
			t.Fatal(panel.NextURL)
		}
		if tc.page == 2 && !strings.Contains(panel.PreviousURL, "backup_page=1") {
			t.Fatal(panel.PreviousURL)
		}
	}
	a.backupMu.Lock()
	a.backupState = backupRuntime{Running: true, Started: 123}
	a.backupMu.Unlock()
	if panel := backupView(t, a, ""); !panel.Running || panel.Started != 123 || panel.Next != 0 {
		t.Fatalf("%+v", panel)
	}
	a.backupMu.Lock()
	a.backupState = backupRuntime{Next: 999}
	a.backupMu.Unlock()
	if panel := backupView(t, a, ""); panel.Next != 999 {
		t.Fatal(panel.Next)
	}
	a.backupMu.Lock()
	a.backupState = backupRuntime{}
	a.backupMu.Unlock()
	for i := range 55 {
		day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102")
		if err := os.WriteFile(filepath.Join(a.config.Backup.Directory, fmt.Sprintf("roi-%s.sqlite", day)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	today := filepath.Join(a.config.Backup.Directory, "roi-"+time.Now().UTC().Format("20060102")+".sqlite")
	if err := os.WriteFile(today, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	panel := backupView(t, a, "")
	if panel.StoredCount != 56 || panel.StoredBytes != 56 || len(panel.Files) != 50 || panel.Next <= time.Now().Unix() {
		t.Fatalf("%+v", panel)
	}
}

func TestBackupAccountingStorageFailureAndMigration(t *testing.T) {
	a := testApp(t)
	a.config.Backup = BackupConfig{Enabled: true, Directory: t.TempDir(), Time: "03:00", Keep: 2}
	mustExec(t, a, "DROP TABLE backup_days")
	if _, err := a.dailyBackup(t.Context(), time.Now()); err == nil || !strings.Contains(err.Error(), "record backup statistics") {
		t.Fatal(err)
	}
	var panel backupPanel
	if err := a.backupStatistics(httptest.NewRequest("GET", "/maintenance", nil), &panel); err == nil {
		t.Fatal("database error hidden")
	}
	if _, err := a.store.DB.Exec("DROP TABLE subscription_backup_state; PRAGMA user_version=13"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	if panel := backupView(t, a, ""); panel.Attempts != 0 {
		t.Fatal(panel)
	}
}
