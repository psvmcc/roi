package roi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func resetTestFeed(t *testing.T, a *App, kind string) int64 {
	t.Helper()
	var id int64
	if kind == "rss" {
		id = addFeed(t, a, "https://rss.example/feed")
		mustExec(t, a, "INSERT INTO folders VALUES('Custom/Folder')")
		mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Custom/Folder')", id)
		mustExec(t, a, "UPDATE feeds SET user_agent='Custom/1',icon_url='https://rss.example/icon',site='https://rss.example/' WHERE id=?", id)
	} else {
		source, err := parseGitSource("https://github.com/team/repo", "github", strings.TrimPrefix(kind, "git-"), true)
		if err != nil {
			t.Fatal(err)
		}
		source.TagFilter = "v*"
		id = storeGit(t, a, source)
	}
	mustExec(t, a, "UPDATE feeds SET title='Custom name' WHERE id=?", id)
	save(t, a, id, &gofeed.Item{GUID: "old", Title: "Old", Content: "old content"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE feed_id=?", id)
	mustExec(t, a, "INSERT INTO tombstones VALUES(?,'rotated')", id)
	mustExec(t, a, "INSERT INTO fetches(feed_id,started,finished,status,added,error) VALUES(?,1,2,503,0,'old error')", id)
	mustExec(t, a, "UPDATE feeds SET etag='etag',modified='date',last_attempt=1,last_success=2,last_new=3,last_published=4,next_fetch=9999999999,error='old error',failures=9 WHERE id=?", id)
	return id
}
func TestResetFeedPreservesSubscriptionAndOnlyClearsSelectedState(t *testing.T) {
	for _, kind := range []string{"rss", "git-releases", "git-tags"} {
		for _, state := range []string{"active", "paused", "disabled"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				a := testApp(t)
				id := resetTestFeed(t, a, kind)
				other := addFeed(t, a, "https://elsewhere.example/feed")
				save(t, a, other, &gofeed.Item{GUID: "untouched", Title: "Keep"})
				mustExec(t, a, "INSERT INTO host_limits VALUES('github.com',9999999999999)")
				mustExec(t, a, "UPDATE feeds SET active=?,paused=? WHERE id=?", state != "disabled", state == "paused", id)
				before, err := a.store.Feeds()
				if err != nil {
					t.Fatal(err)
				}
				var original Feed
				for _, f := range before {
					if f.ID == id {
						original = f
					}
				}
				oldID := feedEntryID(t, a, id)
				if err := a.store.ResetFeed(id); err != nil {
					t.Fatal(err)
				}
				after, err := a.store.Feeds()
				if err != nil {
					t.Fatal(err)
				}
				var reset Feed
				for _, f := range after {
					if f.ID == id {
						reset = f
					}
				}
				if !reset.Active || reset.Paused || reset.Attempt != 0 || reset.Success != 0 || reset.New != 0 || reset.Next != 0 || reset.Published != 0 || reset.Failures != 0 || reset.Error != "" || reset.Count != 0 || reset.Unread != 0 || reset.Bytes != 0 {
					t.Fatal("state not cleared", reset)
				}
				if reset.ID != original.ID || reset.URL != original.URL || reset.Title != original.Title || reset.Site != original.Site || reset.Icon != original.Icon || reset.UserAgent != original.UserAgent || reset.GitProvider != original.GitProvider || reset.GitRepo != original.GitRepo || reset.GitMode != original.GitMode || reset.GitPreview != original.GitPreview || reset.GitTagFilter != original.GitTagFilter || !reflect.DeepEqual(reset.Folders, original.Folders) || reset.GitRevision != original.GitRevision+1 {
					t.Fatal("subscription settings changed", original, reset)
				}
				for _, table := range []string{"entries", "tombstones", "fetches"} {
					if countRows(t, a, table, id) != 0 {
						t.Fatal("state retained", table)
					}
				}
				var etag, modified string
				if err := a.store.DB.QueryRow("SELECT etag,modified FROM feeds WHERE id=?", id).Scan(&etag, &modified); err != nil || etag != "" || modified != "" {
					t.Fatal(etag, modified, err)
				}
				if countRows(t, a, "entries", other) != 1 {
					t.Fatal("another feed was cleared")
				}
				var cooldown int64
				if err := a.store.DB.QueryRow("SELECT blocked_until FROM host_limits WHERE host='github.com'").Scan(&cooldown); err != nil || cooldown != 9999999999999 {
					t.Fatal("reset bypassed host cooldown", cooldown, err)
				}
				if n := save(t, a, id, &gofeed.Item{GUID: "old", Title: "Fresh"}, &gofeed.Item{GUID: "rotated", Title: "Restored"}); n != 2 {
					t.Fatal("reset did not allow fresh download", n)
				}
				var smallest, read, starred int64
				if err := a.store.DB.QueryRow("SELECT min(id),sum(is_read),sum(starred) FROM entries WHERE feed_id=?", id).Scan(&smallest, &read, &starred); err != nil || smallest <= oldID || read != 0 || starred != 0 {
					t.Fatal(smallest, oldID, read, starred, err)
				}
			})
		}
	}
}
func TestResetFeedTransactionRollbackAndErrors(t *testing.T) {
	for _, table := range []string{"feeds", "entries", "tombstones", "fetches"} {
		t.Run(table, func(t *testing.T) {
			a := testApp(t)
			id := resetTestFeed(t, a, "rss")
			operation := "DELETE"
			if table == "feeds" {
				operation = "UPDATE"
			}
			mustExec(t, a, "CREATE TRIGGER fail_reset BEFORE "+operation+" ON "+table+" BEGIN SELECT RAISE(ABORT,'reset storage failure'); END")
			if err := a.store.ResetFeed(id); err == nil {
				t.Fatal("failure ignored")
			}
			for _, table := range []string{"entries", "tombstones", "fetches"} {
				if countRows(t, a, table, id) != 1 {
					t.Fatal("partial reset committed", table)
				}
			}
			var failures, revision int64
			var etag string
			if err := a.store.DB.QueryRow("SELECT failures,git_revision,etag FROM feeds WHERE id=?", id).Scan(&failures, &revision, &etag); err != nil || failures != 9 || revision != 0 || etag != "etag" {
				t.Fatal("reset metadata not rolled back", failures, revision, etag, err)
			}
		})
	}
	a := testApp(t)
	if err := a.store.ResetFeed(999); !errors.Is(err, ErrFeedNotFound) {
		t.Fatal(err)
	}
	a.Close()
	if err := a.store.ResetFeed(1); err == nil {
		t.Fatal("closed database ignored")
	}
}
func TestResetFeedAdminRequiresAuthenticationCSRFAndConfirmation(t *testing.T) {
	a := testApp(t)
	id := resetTestFeed(t, a, "rss")
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "id": {fmt.Sprint(id)}, "confirm": {"reset"}}
	if w := asyncAdmin(a, nil, "POST", "/reset", values); w.Code != 401 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", "wrong")
	if w := asyncAdmin(a, cookie, "POST", "/reset", values); w.Code != 403 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", csrf)
	values.Del("confirm")
	if w := asyncAdmin(a, cookie, "POST", "/reset", values); w.Code != 400 {
		t.Fatal(w.Code)
	}
	values.Set("confirm", "reset")
	for _, invalid := range []string{"bad", "0", "-1", "999999"} {
		values.Set("id", invalid)
		want := 400
		if invalid == "999999" {
			want = 404
		}
		if w := asyncAdmin(a, cookie, "POST", "/reset", values); w.Code != want {
			t.Fatal(invalid, w.Code)
		}
	}
	values.Set("id", fmt.Sprint(id))
	if w := asyncAdmin(a, cookie, "GET", "/reset", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, cookie, "PUT", "/reset", values); w.Code != 405 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/reset", strings.NewReader(values.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := apiReq(a, approvedToken(t, a), "POST", "/reader/api/0/reset", values); w.Code != 404 {
		t.Fatal("Reader API exposed reset", w.Code)
	}
	if countRows(t, a, "entries", id) != 1 {
		t.Fatal("unconfirmed reset removed articles")
	}
	mustExec(t, a, "CREATE TRIGGER fail_reset BEFORE DELETE ON entries BEGIN SELECT RAISE(ABORT,'storage failure'); END")
	if w := asyncAdmin(a, cookie, "POST", "/reset", values); w.Code != 500 {
		t.Fatal(w.Code)
	}
	mustExec(t, a, "DROP TRIGGER fail_reset")
	if w := asyncAdmin(a, cookie, "POST", "/reset", values); w.Code != 200 || !strings.Contains(w.Body.String(), "Feed reset") {
		t.Fatal(w.Code, w.Body.String())
	}
	if countRows(t, a, "entries", id) != 0 {
		t.Fatal("confirmed reset retained articles")
	}
}
func TestResetRejectsInFlightResponsesAndFetchesAgain(t *testing.T) {
	for _, kind := range []string{"rss", "git"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			var id int64
			if kind == "git" {
				source, _ := parseGitSource("https://github.com/team/repo", "github", "releases", false)
				source.TagFilter = "v*"
				id = storeGit(t, a, source)
			} else {
				id = addFeed(t, a, "https://rss.example/feed")
			}
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
				once.Do(func() { close(started) })
				<-release
				body := queueRSS
				if kind == "git" {
					body = `[{"tag_name":"v1","published_at":"2024-01-01T00:00:00Z","body":"# Fresh"}]`
				}
				return gitResponse(r, 200, body), nil
			})
			done := make(chan struct{})
			go func() { defer close(done); a.poller.fetch(t.Context(), id) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("fetch not started")
			}
			// Do not cancel the request: the revision check itself must reject it.
			if err := a.store.ResetFeed(id); err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("stale fetch did not finish")
			}
			if countRows(t, a, "entries", id) != 0 || countRows(t, a, "fetches", id) != 0 {
				t.Fatal("stale response repopulated reset feed")
			}
			a.poller.fetch(t.Context(), id)
			if countRows(t, a, "entries", id) != 1 || countRows(t, a, "fetches", id) != 1 {
				t.Fatal("fresh response not saved")
			}
		})
	}
}
