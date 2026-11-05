package roi

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func subscriptionTestConfig(t *testing.T, a *App) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "subscriptions")
	a.config.SubscriptionBackup = BackupConfig{Enabled: true, Directory: directory, Time: "03:00", Keep: 30}
	return directory
}
func subscriptionView(t *testing.T, a *App) subscriptionBackupPanel {
	t.Helper()
	var panel subscriptionBackupPanel
	if err := a.subscriptionBackupStatistics(httptest.NewRequest("GET", "/maintenance", nil), &panel); err != nil {
		t.Fatal(err)
	}
	return panel
}
func checkSubscriptions(t *testing.T, a *App, now time.Time, want bool) {
	t.Helper()
	created, err := a.checkSubscriptionBackup(t.Context(), now)
	if err != nil || created != want {
		t.Fatal(created, err)
	}
}
func TestSubscriptionBackupDailyChangesRestartAndRestore(t *testing.T) {
	a := testApp(t)
	directory := subscriptionTestConfig(t, a)
	feed := addFeed(t, a, "https://example.org/rss")
	save(t, a, feed, &gofeed.Item{GUID: "1", Title: "Article"})
	now := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	checkSubscriptions(t, a, now, true) // Initial copy before the scheduled time.
	path := filepath.Join(directory, "roi-20261102.roi")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	mustExec(t, a, "UPDATE feeds SET title='Renamed',paused=1,user_agent='Custom/1'")
	checkSubscriptions(t, a, now.Add(4*time.Hour), false)
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, original) {
		t.Fatal("same-day copy changed")
	}
	c := a.config
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	checkSubscriptions(t, reopened, now.Add(5*time.Hour), false)
	checkSubscriptions(t, reopened, now.AddDate(0, 0, 1), false) // Before daily time, no immediate copy after restart.
	checkSubscriptions(t, reopened, now.AddDate(0, 0, 1).Add(3*time.Hour), true)
	// Article state and sync metadata never change the export.
	mustExec(t, reopened, "UPDATE entries SET starred=1,is_read=1,content='Different article',updated=999")
	mustExec(t, reopened, "UPDATE feeds SET last_attempt=999,failures=4,error='HTTP 503',next_fetch=999")
	checkSubscriptions(t, reopened, now.AddDate(0, 0, 2).Add(3*time.Hour), false)
	checkSubscriptions(t, reopened, now.AddDate(0, 0, 3).Add(3*time.Hour), false)
	mustExec(t, reopened, "UPDATE feeds SET active=0,paused=0")
	checkSubscriptions(t, reopened, now.AddDate(0, 0, 4).Add(3*time.Hour), true)
	panel := subscriptionView(t, reopened)
	if panel.Created != 3 || panel.StoredCount != 3 || panel.Error != "" || panel.LastCreated == 0 {
		t.Fatalf("%+v", panel)
	}
	latest, err := os.ReadFile(filepath.Join(directory, "roi-20261106.roi"))
	if err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err := other.store.ImportROI(bytes.NewReader(latest)); err != nil {
		t.Fatal(err)
	}
	feeds, err := other.store.Feeds()
	if err != nil || len(feeds) != 1 || feeds[0].Title != "Renamed" || feeds[0].Paused || feeds[0].Active || feeds[0].UserAgent != "Custom/1" {
		t.Fatal(feeds, err)
	}
	cookie, _ := adminSession(t, reopened)
	page := adminRequest(reopened, cookie, "GET", "/maintenance?fragment=1", nil)
	for _, text := range []string{`id="subscription-backups"`, "Subscription backups", "3 copies", "roi-20261106.roi", "keep 30 copies"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), text) {
			t.Fatal(text, page.Code)
		}
	}
}

func TestSubscriptionBackupRetentionAndDeterministicExport(t *testing.T) {
	a := testApp(t)
	directory := subscriptionTestConfig(t, a)
	a.config.SubscriptionBackup.Keep = 2
	id := addFeed(t, a, "https://example.org/b")
	addFeed(t, a, "https://example.org/a")
	mustExec(t, a, "INSERT INTO folders VALUES('B'),('A')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'B'),(?,'A')", id, id)
	var before, after bytes.Buffer
	if err := a.store.ExportROI(&before); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "DELETE FROM feed_folders;INSERT INTO feed_folders VALUES(?,'A'),(?,'B')", id, id)
	if err := a.store.ExportROI(&after); err != nil || !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("folder ordering unstable", err)
	}
	now := time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC)
	for i := range 4 {
		mustExec(t, a, "UPDATE feeds SET title=? WHERE id=?", fmt.Sprintf("Version %d", i), id)
		checkSubscriptions(t, a, now.AddDate(0, 0, i), true)
	}
	for _, name := range []string{"manual.roi", "roi-20261301.roi", "roi-20200101.sqlite"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(directory, "roi-20260104.roi"), filepath.Join(directory, "roi-20000101.roi")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a.config.Database, filepath.Join(directory, "roi-20000102.roi")); err != nil {
		t.Fatal(err)
	}
	checkSubscriptions(t, a, now.AddDate(0, 0, 4), false)
	panel := subscriptionView(t, a)
	if panel.StoredCount != 2 || panel.Created != 4 || panel.Bytes <= panel.StoredBytes || len(panel.Files) != 2 || panel.Result != "Unchanged" {
		t.Fatalf("%+v", panel)
	}
	if _, err := os.Stat(filepath.Join(directory, "roi-20260101.roi")); !os.IsNotExist(err) {
		t.Fatal("old ROI copy retained", err)
	}
	for _, name := range []string{"manual.roi", "roi-20261301.roi", "roi-20200101.sqlite", "roi-20000101.roi", "roi-20000102.roi"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubscriptionBackupFailuresRetryAndExistingFiles(t *testing.T) {
	a := testApp(t)
	directory := subscriptionTestConfig(t, a)
	now := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "roi-20261102.roi")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkSubscriptionBackup(t.Context(), now); err == nil {
		t.Fatal("invalid daily file accepted")
	}
	if panel := subscriptionView(t, a); panel.Error == "" || panel.Created != 0 {
		t.Fatalf("%+v", panel)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checkSubscriptions(t, a, now, true)
	if panel := subscriptionView(t, a); panel.Error != "" || panel.Created != 1 {
		t.Fatalf("%+v", panel)
	}
	// A valid same-day file is verified rather than overwritten if the DB state is absent.
	mustExec(t, a, "DELETE FROM subscription_backup_state")
	checkSubscriptions(t, a, now, false)
	if panel := subscriptionView(t, a); panel.Result != "Today's copy already exists" || panel.Created != 0 {
		t.Fatal(panel.Result)
	}
	mustExec(t, a, "DELETE FROM subscription_backup_state")
	next := now.AddDate(0, 0, 1)
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkSubscriptionBackup(t.Context(), next); err == nil {
		t.Fatal("invalid previous backup accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.config.Database, filepath.Join(directory, "roi-20261103.roi")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkSubscriptionBackup(t.Context(), next); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(filepath.Join(directory, "roi-20261103.roi")); err != nil {
		t.Fatal(err)
	}
	checkSubscriptions(t, a, next, true)
	mustExec(t, a, "UPDATE subscription_backup_state SET error='retry',checked_day='' ")
	if err := os.Remove(filepath.Join(directory, "roi-20261103.roi")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.checkSubscriptionBackup(t.Context(), next); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "blocked"), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a.config.SubscriptionBackup.Directory = filepath.Join(directory, "blocked")
	if _, err := a.checkSubscriptionBackup(t.Context(), next); err == nil {
		t.Fatal("directory failure ignored")
	}
	if panel := subscriptionView(t, a); panel.ScanError == "" || panel.Error == "" {
		t.Fatalf("%+v", panel)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.checkSubscriptionBackup(canceled, next); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := publishSubscriptionBackup(canceled, directory, filepath.Join(directory, "cancel.roi"), []byte("x")); err == nil {
		t.Fatal("publication cancellation ignored")
	}
	if _, err := publishSubscriptionBackup(t.Context(), directory, filepath.Join(directory, "blocked"), []byte("new")); err == nil {
		t.Fatal("existing destination overwritten")
	}
	if _, err := readSubscriptionBackup(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := pruneSubscriptionBackups(t.Context(), filepath.Join(directory, "blocked"), 1, a.config.Database); err == nil {
		t.Fatal("rotation failure ignored")
	}
}

func TestSubscriptionBackupSchedulerConcurrencyConfigAndPanel(t *testing.T) {
	a := testApp(t)
	if panel := subscriptionView(t, a); panel.Enabled {
		t.Fatal("disabled panel visible")
	}
	a.runSubscriptionBackups(t.Context())
	defaults := a.config.subscriptionBackupConfig()
	if defaults.Keep != 30 || defaults.Time != "03:00" || !strings.HasSuffix(defaults.Directory, "subscriptions") {
		t.Fatal(defaults)
	}
	for _, b := range []BackupConfig{{Time: "25:00"}, {Keep: -1}, {Directory: " "}} {
		c := testConfig(t)
		c.SubscriptionBackup = b
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "subscription_backup.") {
			t.Fatal(b, err)
		}
	}
	directory := subscriptionTestConfig(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.runSubscriptionBackups(ctx) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(directory, "roi-"+time.Now().UTC().Format("20060102")+".roi")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("initial snapshot missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		a.backupMu.Lock()
		next := a.subscriptionState.Next
		a.backupMu.Unlock()
		if next > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("scheduler did not schedule next check")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	now := time.Now().UTC().AddDate(0, 0, 1)
	addFeed(t, a, "https://example.org/new")
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	for range 5 {
		wg.Go(func() {
			created, err := a.checkSubscriptionBackup(t.Context(), backupTime(now, "04:00"))
			if err != nil {
				t.Error(err)
			}
			if created {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if createdCount != 1 {
		t.Fatal(createdCount)
	}
	a.backupMu.Lock()
	a.subscriptionState = backupRuntime{Running: true, Started: 123}
	a.backupMu.Unlock()
	if panel := subscriptionView(t, a); !panel.Running || panel.Next != 0 {
		t.Fatal(panel)
	}
	a.backupMu.Lock()
	a.subscriptionState = backupRuntime{}
	a.backupMu.Unlock()
	panel := subscriptionView(t, a)
	if panel.Next == 0 || panel.LastChecked == 0 {
		t.Fatal(panel)
	}
	for i := range 52 {
		day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102")
		if err := os.WriteFile(filepath.Join(directory, "roi-"+day+".roi"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if panel := subscriptionView(t, a); panel.StoredCount != 54 || len(panel.Files) != 50 {
		t.Fatal(panel.StoredCount, len(panel.Files))
	}
	a.Close()
	if _, err := a.checkSubscriptionBackup(t.Context(), now); err == nil {
		t.Fatal("closed DB accepted")
	}
	var broken subscriptionBackupPanel
	if err := a.subscriptionBackupStatistics(httptest.NewRequest("GET", "/maintenance", nil), &broken); err == nil {
		t.Fatal("closed DB hidden")
	}
}

func TestSubscriptionBackupSchedulerRetriesAndRecordingFailures(t *testing.T) {
	a := testApp(t)
	directory := subscriptionTestConfig(t, a)
	block := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(block, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	a.config.SubscriptionBackup.Directory = block
	a.config.SubscriptionBackup.Time = "23:59"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.runSubscriptionBackups(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.backupMu.Lock()
		next := a.subscriptionState.Next
		a.backupMu.Unlock()
		if next > 0 {
			if next < time.Now().Add(14*time.Minute).Unix() {
				cancel()
				t.Fatal("retry scheduled too soon", next)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("retry not scheduled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	a.config.SubscriptionBackup.Directory = directory
	now := time.Date(2026, 11, 2, 4, 0, 0, 0, time.UTC)
	mustExec(t, a, "CREATE TRIGGER fail_subscription_status BEFORE INSERT ON subscription_backup_state BEGIN SELECT RAISE(FAIL,'status unavailable'); END")
	created, err := a.checkSubscriptionBackup(t.Context(), now)
	if !created || err == nil || !strings.Contains(err.Error(), "record subscription backup status") {
		t.Fatal(created, err)
	}
	mustExec(t, a, "DROP TRIGGER fail_subscription_status")
	checkSubscriptions(t, a, now, false)
	files, _, err := subscriptionFiles(t.Context(), directory, a.config.Database)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if _, _, err := subscriptionFiles(t.Context(), directory, filepath.Join(directory, "missing.db")); err == nil {
		t.Fatal("missing source ignored")
	}
	if _, err := publishSubscriptionBackup(t.Context(), filepath.Join(directory, "missing"), filepath.Join(directory, "new.roi"), []byte("x")); err == nil {
		t.Fatal("missing directory accepted")
	}
	path := filepath.Join(directory, files[0].Name)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, err := readSubscriptionBackup(path); err == nil {
			t.Fatal("unreadable copy accepted")
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}

type failROIEncoding struct{ writes int }

func (w *failROIEncoding) Write(data []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, fmt.Errorf("encoder destination unavailable")
	}
	return len(data), nil
}

func TestROIExportSnapshotReadAndEncodingFailures(t *testing.T) {
	a := testApp(t)
	addFeed(t, a, "https://example.org/a")
	if err := a.store.ExportROI(&failROIEncoding{}); err == nil {
		t.Fatal("encoding error ignored")
	}
	mustExec(t, a, "UPDATE feeds SET active='corrupt'")
	var out bytes.Buffer
	if err := a.store.ExportROI(&out); err == nil {
		t.Fatal("invalid feed state ignored")
	}
	mustExec(t, a, "UPDATE feeds SET active=1; ALTER TABLE feed_folders RENAME TO real_feed_folders; CREATE VIEW feed_folders AS SELECT 'invalid' AS feed_id,'folder' AS name")
	if err := a.store.ExportROI(&out); err == nil {
		t.Fatal("invalid folder state ignored")
	}
	mustExec(t, a, "DROP VIEW feed_folders")
	if err := a.store.ExportROI(&out); err == nil {
		t.Fatal("missing folder table ignored")
	}
	mustExec(t, a, "DROP TABLE real_feed_folders;DROP TABLE feeds")
	if err := a.store.ExportROI(&out); err == nil {
		t.Fatal("missing feeds table ignored")
	}
}
