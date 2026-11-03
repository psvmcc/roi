package roi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func assertFeedAggregates(t *testing.T, a *App) {
	t.Helper()
	feeds, err := a.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		var count, unread, visible, bytes int64
		err = a.store.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(e.is_read=0),0),COALESCE(SUM(e.is_read=0 AND (f.git_preview=1 OR e.git_preview=0) AND (f.git_tag_filter='' OR substr(e.guid,5) GLOB f.git_tag_filter)),0),COALESCE(SUM(length(CAST(e.title AS BLOB))+length(CAST(e.url AS BLOB))+length(CAST(e.content AS BLOB))+length(CAST(e.author AS BLOB))+length(CAST(e.guid AS BLOB))),0) FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE f.id=?`, f.ID).Scan(&count, &unread, &visible, &bytes)
		if err != nil || count != f.Count || unread != f.Unread || visible != f.VisibleUnread || bytes != f.Bytes {
			t.Fatal(f, count, unread, visible, bytes, err)
		}
	}
}
func TestFeedAggregatesCoverStateChangesAndMigrations(t *testing.T) {
	a := testApp(t)
	first := addFeed(t, a, "https://example.org/one")
	second := addFeed(t, a, "https://example.org/two")
	save(t, a, first, &gofeed.Item{GUID: "git:v1", Title: "α", Content: "Hello"}, &gofeed.Item{GUID: "git:v2-rc1", Title: "Preview", Custom: map[string]string{"roi_preview": "1"}})
	assertFeedAggregates(t, a)
	for _, q := range []string{"UPDATE entries SET is_read=1 WHERE guid='git:v1'", "UPDATE entries SET title='Longer',content='Different',author='Author'", "UPDATE feeds SET git_preview=1", "UPDATE feeds SET git_tag_filter='v1'", "UPDATE entries SET is_read=0,git_preview=0", "UPDATE entries SET feed_id=2 WHERE guid='git:v1'", "UPDATE entries SET guid='git:v3' WHERE feed_id=2", "DELETE FROM entries WHERE feed_id=1"} {
		mustExec(t, a, q)
		assertFeedAggregates(t, a)
	}
	mustExec(t, a, "INSERT INTO folders VALUES('Second'); INSERT INTO feed_folders VALUES(?,'Second')", second)
	feeds, _ := a.store.Feeds()
	if len(feeds[1].Folders) != 1 {
		t.Fatal(feeds)
	}
	rollbackReviewMigration(t, a.store.DB)
	mustExec(t, a, "PRAGMA user_version=10")
	if err := migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	assertFeedAggregates(t, a)
	tx, err := a.store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE entries SET content='Rolled back'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFeedAggregates(t, a)
	mustExec(t, a, "UPDATE feeds SET active=0 WHERE id=?", second)
	if err = a.store.DeleteFeed(second); err != nil {
		t.Fatal(err)
	}
	assertFeedAggregates(t, a)
}
func TestFilteredItemsCannotBeChangedByKnownIDs(t *testing.T) {
	a, srv := fixtureGit(t, "forgejo")
	s, _ := parseGitSource(srv.URL+"/owner/repo", "forgejo", "releases", true)
	id := storeGit(t, a, s)
	a.poller.fetch(t.Context(), id)
	var old int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=? AND guid='git:v1.0.0'", id).Scan(&old); err != nil {
		t.Fatal(err)
	}
	s.TagFilter = "v2.*"
	if err := a.store.SaveGitSource(id, s); err != nil {
		t.Fatal(err)
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"i": {fmt.Sprint(old)}, "a": {statePrefix + "starred", statePrefix + "read"}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var read, starred bool
	if err := a.store.DB.QueryRow("SELECT is_read,starred FROM entries WHERE id=?", old).Scan(&read, &starred); err != nil || read || starred {
		t.Fatal(read, starred, err)
	}
	cookie, _ := adminSession(t, a)
	body := adminRequest(a, cookie, "GET", "/", nil).Body.String()
	if !strings.Contains(body, `<span>Unread</span><strong>1</strong>`) || !strings.Contains(body, "3 stored") {
		t.Fatal("dashboard totals do not match visibility")
	}
}
func TestSessionPaginationAndPendingBound(t *testing.T) {
	a := testApp(t)
	token := approvedToken(t, a)
	for i := 0; i < 125; i++ {
		raw := fmt.Sprintf("approved-%d", i)
		if err := a.createSession(raw, "api", time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
		if err := a.nameSession(sessionID(raw), fmt.Sprintf("Device %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 150; i++ {
		if err := a.createSession(fmt.Sprintf("pending-%d", i), "api", time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var pending int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM auth_sessions WHERE realm='api' AND approved=0 AND revoked=0").Scan(&pending); err != nil || pending != 100 {
		t.Fatal(pending, err)
	}
	if !a.validToken(token) {
		t.Fatal("approved device was evicted")
	}
	all, err := a.sessionList("")
	if err != nil || len(all) != 226 {
		t.Fatal(len(all), err)
	}
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		rows, err := a.querySessions("", "api", page)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatal("duplicate", row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 226 || !seen[sessionID(token)] {
		t.Fatal(len(seen))
	}
	cookie, _ := adminSession(t, a)
	for _, page := range []string{"1", "2", "3", "-100", "999999999999999999999"} {
		body := adminRequest(a, cookie, "GET", "/?reader_page="+page, nil).Body.String()
		if !strings.Contains(body, "226 active sessions") || !strings.Contains(body, "data-session-page") {
			t.Fatal("pagination missing", page)
		}
	}
}
func TestTombstoneDigestsAndCompactionPreserveSuppression(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	long := strings.Repeat("long-guid", 1000)
	save(t, a, id, &gofeed.Item{GUID: long}, &gofeed.Item{GUID: "keep"})
	mustExec(t, a, "UPDATE entries SET received=1 WHERE guid=?", long)
	a.config.Retention.Months = 1
	if n, err := a.store.Rotate(a.config, time.Now()); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var size int
	if err := a.store.DB.QueryRow("SELECT length(guid) FROM tombstones WHERE feed_id=?", id).Scan(&size); err != nil || size != 32 {
		t.Fatal(size, err)
	}
	if n := save(t, a, id, &gofeed.Item{GUID: long}); n != 0 {
		t.Fatal("resurrected retired article")
	}
	mustExec(t, a, "INSERT INTO tombstones VALUES(?,'legacy-guid')", id)
	if err := a.store.CompactTombstones(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := save(t, a, id, &gofeed.Item{GUID: "legacy-guid"}); n != 0 {
		t.Fatal("resurrected legacy article")
	}
	usage, err := a.store.Space(t.Context())
	if err != nil || usage.Tombstones != 2 || usage.TombstoneBytes != 64 {
		t.Fatal(usage, err)
	}
	assertFeedAggregates(t, a)
	w := httptest.NewRecorder()
	a.OpsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "roi_tombstone_key_bytes 64") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = CompactDatabase(t.Context(), a.config); err == nil {
		t.Fatal("compacted database used by running app")
	}
	c := a.config
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := CompactDatabase(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(backup); err != nil {
		t.Fatal(err)
	}
	restored, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if n := save(t, restored, id, &gofeed.Item{GUID: long}); n != 0 {
		t.Fatal("VACUUM lost suppression")
	}
	if err = restored.store.DB.QueryRow("SELECT COUNT(*) FROM entries WHERE feed_id=?", id).Scan(&size); err != nil || size != 1 {
		t.Fatal(size, err)
	}
	usage, err = restored.store.Space(t.Context())
	if err != nil || usage.FreeBytes != 0 {
		t.Fatal(usage, err)
	}
}
func TestBackupRestoreStartsApplicationWithStateAndSessions(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "persist", Title: "Saved article", Content: "Saved body"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1")
	token := approvedToken(t, a)
	cookie, _ := adminSession(t, a)
	directory := t.TempDir()
	backup := filepath.Join(directory, "backup.sqlite")
	if err := writeBackup(t.Context(), a.config.Database, directory, backup); err != nil {
		t.Fatal(err)
	}
	c := a.config
	c.Database = filepath.Join(t.TempDir(), "restored.sqlite")
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Database, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.validToken(token) {
		t.Fatal("Reader session did not survive restore")
	}
	w := apiReq(restored, token, "GET", "/reader/api/0/stream/contents/reading-list", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Saved body") || !strings.Contains(w.Body.String(), "state/com.google/read") || !strings.Contains(w.Body.String(), "state/com.google/starred") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = adminRequest(restored, cookie, "GET", "/", nil); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	assertFeedAggregates(t, restored)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = restored.Run(ctx); err != nil {
		t.Fatal("restored application could not start", err)
	}
}
