package roi

import (
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/crypto/bcrypt"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	h, e := bcrypt.GenerateFromPassword([]byte("test-password-123"), bcrypt.MinCost)
	if e != nil {
		t.Fatal(e)
	}
	var c Config
	c.API = Endpoint{Listen: "127.0.0.1:18080", Prefix: "/greader"}
	c.Admin.Listen = "127.0.0.1:18081"
	c.Ops.Listen = "127.0.0.1:18082"
	c.Database = filepath.Join(t.TempDir(), "roi.db")
	if err := os.Chmod(filepath.Dir(c.Database), 0700); err != nil {
		t.Fatal(err)
	}
	c.AdminAuth = Credentials{"admin", string(h)}
	c.APIAuth = Credentials{"reader", string(h)}
	c.Poll.Interval = "1s"
	c.Poll.PerHostDelay = "0s"
	c.Poll.Timeout = "2s"
	c.Poll.Workers = 2
	c.Poll.MaxBytes = 1 << 20
	c.Poll.AllowPrivate = true
	c.Retention.KeepStarred = true
	return c
}

func testApp(t *testing.T) *App {
	t.Helper()
	a, e := New(testConfig(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := a.Close(); e != nil {
			t.Error(e)
		}
	})
	return a
}

func mustExec(t *testing.T, a *App, q string, args ...any) {
	t.Helper()
	if strings.Contains(q, "PRAGMA user_version=") && !strings.Contains(q, "user_version=999") {
		rollbackReviewMigration(t, a.store.DB)
	}
	if _, e := a.store.DB.Exec(q, args...); e != nil {
		t.Fatal(e)
	}
}

// Migration fixtures explicitly remove all newer derived objects before
// reconstructing the historical schema they intend to exercise.
func rollbackReviewMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 11 {
		return
	}
	if version >= 15 {
		if _, err := db.Exec("DROP TABLE subscription_backup_state"); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 14 {
		if _, err := db.Exec("DROP TABLE backup_days"); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 13 {
		if _, err := db.Exec("DROP INDEX auth_sessions_device; ALTER TABLE auth_sessions DROP COLUMN device_id; DROP TABLE api_devices"); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 12 {
		if _, err := db.Exec("ALTER TABLE auth_sessions DROP COLUMN feeds_hidden"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.Exec(`DROP TRIGGER feed_stats_insert; DROP TRIGGER entry_stats_insert; DROP TRIGGER entry_stats_delete; DROP TRIGGER entry_stats_update; DROP TRIGGER feed_stats_filter; DROP TRIGGER tombstone_stats_insert; DROP TRIGGER tombstone_stats_delete; DROP TABLE feed_stats; DROP TABLE git_http_cache; DROP INDEX entries_feed_published; DROP INDEX entries_updated; ALTER TABLE entries DROP COLUMN git_object; ALTER TABLE entries DROP COLUMN render_version; PRAGMA user_version=10;`)
	if err != nil {
		t.Fatal(err)
	}
}

func addFeed(t *testing.T, a *App, raw string) int64 {
	t.Helper()
	res, e := a.store.DB.Exec("INSERT INTO feeds(url,title) VALUES(?,?)", raw, "Example")
	if e != nil {
		t.Fatal(e)
	}
	id, e := res.LastInsertId()
	if e != nil {
		t.Fatal(e)
	}
	return id
}

func save(t *testing.T, a *App, id int64, items ...*gofeed.Item) int {
	t.Helper()
	n, e := a.poller.saveEntries(context.Background(), id, &gofeed.Feed{Link: "https://example.org", Items: items}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return n
}

func apiReq(a *App, token, method, path string, v url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader(v.Encode())
	} else if len(v) > 0 {
		path += "?" + v.Encode()
	}
	r := httptest.NewRequest(method, "/greader"+path, body)
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		r.Header.Set("Authorization", "GoogleLogin auth="+token)
	}
	w := httptest.NewRecorder()
	a.APIHandler().ServeHTTP(w, r)
	return w
}

func approvedToken(t *testing.T, a *App) string {
	t.Helper()
	token := a.issueToken()
	if token != "" {
		if err := a.nameSession(sessionID(token), "Test device", true); err != nil {
			t.Fatal(err)
		}
	}
	return token
}

func deviceLoginForm(t *testing.T, a *App) url.Values {
	t.Helper()
	c, err := a.store.CreateAPIDevice(t.Context(), "Test device")
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"Email": {c.Login}, "Passwd": {c.Code}}
}
