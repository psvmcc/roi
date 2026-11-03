package roi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOnlineDailyBackupsWALRetentionAndRestart(t *testing.T) {
	a := testApp(t)
	a.config.Backup = BackupConfig{Enabled: true, Directory: filepath.Join(t.TempDir(), "copies"), Time: "03:00", Keep: 2}
	id := addFeed(t, a, "https://example.org/feed")
	mustExec(t, a, "UPDATE feeds SET title='WAL data' WHERE id=?", id)
	now := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	for day := range 3 {
		created, err := a.dailyBackup(t.Context(), now.AddDate(0, 0, day))
		if err != nil || !created {
			t.Fatal(created, err)
		}
	}
	dir := a.config.Backup.Directory
	if _, err := os.Stat(filepath.Join(dir, "roi-20261102.sqlite")); !os.IsNotExist(err) {
		t.Fatal("old backup retained", err)
	}
	path := filepath.Join(dir, "roi-20261104.sqlite")
	db, err := readOnlyDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil || title != "WAL data" {
		t.Fatal(title, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	mustExec(t, a, "UPDATE feeds SET title='After snapshot' WHERE id=?", id)
	if created, err := a.dailyBackup(t.Context(), now.AddDate(0, 0, 2)); err != nil || created {
		t.Fatal("same-day backup overwritten", created, err)
	}
	if err := db.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil || title != "WAL data" {
		t.Fatal(title, err)
	}
	for _, name := range []string{"manual.sqlite", "roi-before-change.db", "roi-20261301.sqlite"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(path, filepath.Join(dir, "roi-20250101.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dailyBackup(t.Context(), now.AddDate(0, 0, 3)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manual.sqlite", "roi-before-change.db", "roi-20261301.sqlite", "roi-20250101.sqlite"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatal(name, err)
		}
	}
}
func TestBackupFailuresDoNotRotateOrPublish(t *testing.T) {
	a := testApp(t)
	dir := t.TempDir()
	a.config.Backup = BackupConfig{Directory: dir, Keep: 1}
	now := time.Now().UTC()
	old := filepath.Join(dir, "roi-20000101.sqlite")
	if err := os.WriteFile(old, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.dailyBackup(canceled, now); err == nil {
		t.Fatal("canceled backup succeeded")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("old backup removed on failure", err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("partial backup left behind", files)
	}
	destination := filepath.Join(dir, "roi-"+now.Format("20060102")+".sqlite")
	if err := os.WriteFile(destination, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dailyBackup(t.Context(), now); err == nil {
		t.Fatal("corrupt existing backup accepted")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.config.Database, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dailyBackup(t.Context(), now); err == nil {
		t.Fatal("source symlink accepted")
	}
	a.config.Backup.Directory = old
	if _, err := a.dailyBackup(t.Context(), now); err == nil {
		t.Fatal("non-directory accepted")
	}
	if err := checkBackup(t.Context(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing backup accepted")
	}
}
func TestBackupSchedulerCatchUpCancellationAndConfig(t *testing.T) {
	a := testApp(t)
	a.config.Backup = BackupConfig{Enabled: true, Directory: t.TempDir(), Time: "00:00", Keep: 2}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.runBackups(ctx) }()
	path := filepath.Join(a.config.Backup.Directory, "roi-"+time.Now().UTC().Format("20060102")+".sqlite")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("catch-up backup missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	a.config.Backup.Enabled = false
	a.runBackups(t.Context())
	for _, b := range []BackupConfig{{Time: "25:00"}, {Time: "3:00"}, {Keep: -1}, {Directory: " "}} {
		c := testConfig(t)
		c.Backup = b
		if err := c.Validate(); err == nil {
			t.Fatal(b)
		}
	}
	at := backupTime(time.Date(2026, 11, 2, 1, 0, 0, 0, time.FixedZone("+2", 7200)), "03:00")
	if at.Format(time.RFC3339) != "2026-11-01T03:00:00Z" {
		t.Fatal(at)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if a.metrics.GetOrCreateCounter("roi_backups_total").Get() != 1 {
		t.Fatal("backup metric missing")
	}
	if !strings.HasSuffix(path, ".sqlite") {
		t.Fatal(path)
	}
}

func TestOnlineBackupWhileWriterTransactionIsOpen(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	tx, err := a.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE feeds SET title='Uncommitted' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	directory := t.TempDir()
	destination := filepath.Join(directory, "copy.sqlite")
	if err := writeBackup(ctx, a.config.Database, directory, destination); err != nil {
		t.Fatal("online backup blocked by live writer", err)
	}
	db, err := readOnlyDatabase(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil || title == "Uncommitted" {
		t.Fatal("snapshot leaked uncommitted data", title, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(destination)
	if err := writeBackup(t.Context(), a.config.Database, directory, destination); err == nil {
		t.Fatal("existing backup overwritten")
	}
	after, _ := os.ReadFile(destination)
	if string(after) != string(original) {
		t.Fatal("atomic publication changed existing file")
	}
	if err := writeBackup(t.Context(), filepath.Join(directory, "missing.db"), directory, filepath.Join(directory, "missing.sqlite")); err == nil {
		t.Fatal("missing source accepted")
	}
}
