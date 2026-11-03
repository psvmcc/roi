package roi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func adminSession(t *testing.T, a *App) (*http.Cookie, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {"test-password-123"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	session, err := a.loadBrowserSession(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	csrf := session.CSRF
	return cookie, csrf
}
func adminRequest(a *App, c *http.Cookie, method, path string, v url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	return w
}
func feedEntryID(t *testing.T, a *App, id int64) int64 {
	t.Helper()
	var item int64
	if e := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=?", id).Scan(&item); e != nil {
		t.Fatal(e)
	}
	return item
}
func countRows(t *testing.T, a *App, table string, id int64) int {
	t.Helper()
	var n int
	if e := a.store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE feed_id=?", id).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestDisabledSubscriptionsHiddenEverywhere(t *testing.T) {
	a := testApp(t)
	active := addFeed(t, a, "https://example.org/active")
	disabled := addFeed(t, a, "https://example.org/disabled")
	save(t, a, active, &gofeed.Item{GUID: "visible", Title: "VISIBLE"})
	save(t, a, disabled, &gofeed.Item{GUID: "hidden", Title: "HIDDEN"})
	hidden := feedEntryID(t, a, disabled)
	mustExec(t, a, "UPDATE entries SET starred=1 WHERE feed_id=?", disabled)
	mustExec(t, a, "INSERT INTO folders VALUES('HiddenFolder')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'HiddenFolder')", disabled)
	token := approvedToken(t, a)
	w := apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"ac": {"unsubscribe"}, "s": {fmt.Sprintf("feed/%d", disabled)}, "T": {"x"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"subscription/list", "tag/list", "unread-count"} {
		w := apiReq(a, token, "GET", "/reader/api/0/"+path, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "HIDDEN") || strings.Contains(w.Body.String(), "HiddenFolder") || strings.Contains(w.Body.String(), fmt.Sprintf(`"id":"feed/%d"`, disabled)) {
			t.Fatal(path, w.Body.String())
		}
	}
	for _, stream := range []string{statePrefix + "reading-list", statePrefix + "starred", statePrefix + "read", statePrefix + "unread", statePrefix + "kept-unread", fmt.Sprintf("feed/%d", disabled), "user/1/label/HiddenFolder"} {
		t.Run(stream, func(t *testing.T) {
			for _, path := range []string{"stream/items/ids", "stream/contents/" + stream} {
				w := apiReq(a, token, "GET", "/reader/api/0/"+path, url.Values{"s": {stream}})
				if w.Code != 200 || strings.Contains(w.Body.String(), "HIDDEN") || strings.Contains(w.Body.String(), fmt.Sprintf(`"id":"%d"`, hidden)) || strings.Contains(w.Body.String(), entryID(hidden)) {
					t.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
	for _, raw := range []string{fmt.Sprint(hidden), entryID(hidden), fmt.Sprintf("%016x", hidden)} {
		w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {raw}})
		if w.Code != 200 || strings.Contains(w.Body.String(), "HIDDEN") {
			t.Fatal(w.Body.String())
		}
	}
	for _, action := range []url.Values{{"i": {fmt.Sprint(hidden)}, "a": {statePrefix + "read"}}, {"i": {fmt.Sprint(hidden)}, "r": {statePrefix + "starred"}}} {
		w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", action)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	w = apiReq(a, token, "POST", "/reader/api/0/mark-all-as-read", url.Values{"s": {fmt.Sprintf("feed/%d", disabled)}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var read, starred bool
	if e := a.store.DB.QueryRow("SELECT is_read,starred FROM entries WHERE id=?", hidden).Scan(&read, &starred); e != nil || read || !starred {
		t.Fatal("disabled entry was modified", e)
	}
	w = apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"s": {fmt.Sprintf("feed/%d", disabled)}, "ac": {"edit"}, "t": {"changed"}})
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	var opml bytes.Buffer
	if e := a.store.Export(&opml); e != nil || strings.Contains(opml.String(), "/disabled") {
		t.Fatal(opml.String(), e)
	}
	fs, e := a.store.Feeds()
	if e != nil || len(fs) != 2 {
		t.Fatal(fs, e)
	}
	if countRows(t, a, "entries", disabled) != 1 {
		t.Fatal("unsubscribe deleted history")
	}
	if e = a.store.SetEnabled(disabled, true); e != nil {
		t.Fatal(e)
	}
	w = apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {fmt.Sprint(hidden)}})
	if !strings.Contains(w.Body.String(), "HIDDEN") {
		t.Fatal("enable did not restore content")
	}
}

func TestAdminDisableEnableAndPermanentDelete(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/delete")
	save(t, a, id, &gofeed.Item{GUID: "one", Title: "Article"})
	mustExec(t, a, "INSERT INTO tombstones VALUES(?,'old')", id)
	mustExec(t, a, "INSERT INTO fetches(feed_id,started,finished,status,added,error) VALUES(?,1,2,200,1,'')", id)
	mustExec(t, a, "INSERT INTO folders VALUES('Dev')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Dev')", id)
	c, csrf := adminSession(t, a)
	form := url.Values{"id": {fmt.Sprint(id)}, "csrf": {csrf}}
	if w := adminRequest(a, c, "POST", "/delete", form); w.Code != 409 {
		t.Fatal("deleted enabled feed", w.Code)
	}
	if w := adminRequest(a, c, "GET", "/delete?id="+fmt.Sprint(id), nil); w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/disable", "/enable", "/delete"} {
		if w := adminRequest(a, c, "POST", path, url.Values{"id": {fmt.Sprint(id)}}); w.Code != 403 {
			t.Fatal("missing CSRF accepted", path, w.Code)
		}
		if w := adminRequest(a, nil, "POST", path, form); w.Code != 303 {
			t.Fatal("unauthenticated mutation", path, w.Code)
		}
	}
	if w := adminRequest(a, c, "POST", "/disable", form); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	if w := adminRequest(a, c, "GET", "/?disabled=1", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Disabled") || !strings.Contains(w.Body.String(), "Delete…") {
		t.Fatal(w.Body.String())
	}
	if w := adminRequest(a, c, "GET", "/delete?id="+fmt.Sprint(id), nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Delete permanently") {
		t.Fatal(w.Code, w.Body.String())
	}
	if countRows(t, a, "entries", id) != 1 {
		t.Fatal("GET deleted data")
	}
	if w := adminRequest(a, c, "POST", "/enable", form); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	fs, e := a.store.Feeds()
	if e != nil || !fs[0].Active || fs[0].Paused || fs[0].Count != 1 {
		t.Fatal(fs, e)
	}
	if w := adminRequest(a, c, "POST", "/disable", form); w.Code != 303 {
		t.Fatal(w.Body.String())
	}
	if w := adminRequest(a, c, "POST", "/delete", form); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, table := range []string{"entries", "fetches", "tombstones", "feed_folders"} {
		if n := countRows(t, a, table, id); n != 0 {
			t.Fatal("orphan rows", table, n)
		}
	}
	fs, e = a.store.Feeds()
	if e != nil || len(fs) != 0 {
		t.Fatal(fs, e)
	}
	if w := adminRequest(a, c, "POST", "/delete", form); w.Code != 404 {
		t.Fatal("missing feed", w.Code)
	}
	for _, path := range []string{"/disable", "/enable", "/delete"} {
		if w := adminRequest(a, c, "POST", path, url.Values{"id": {"bad"}, "csrf": {csrf}}); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
		if w := adminRequest(a, c, "POST", path, url.Values{"id": {"999"}, "csrf": {csrf}}); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/delete?id=bad", "/delete?id=999", "/delete?id=0"} {
		w := adminRequest(a, c, "GET", path, nil)
		if w.Code != 400 && w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestDeleteTransactionRollback(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rollback")
	save(t, a, id, &gofeed.Item{GUID: "one"})
	mustExec(t, a, "INSERT INTO tombstones VALUES(?,'old')", id)
	mustExec(t, a, "INSERT INTO fetches(feed_id,started,finished,status,added,error) VALUES(?,1,2,200,1,'')", id)
	if e := a.store.SetEnabled(id, false); e != nil {
		t.Fatal(e)
	}
	mustExec(t, a, `CREATE TRIGGER prevent_delete BEFORE DELETE ON entries BEGIN SELECT RAISE(ABORT,'test rollback'); END`)
	if e := a.store.DeleteFeed(id); e == nil {
		t.Fatal("expected trigger failure")
	}
	for _, table := range []string{"entries", "fetches", "tombstones"} {
		if countRows(t, a, table, id) != 1 {
			t.Fatal("partial deletion", table)
		}
	}
	if e := a.store.DeleteFeed(999); !errors.Is(e, ErrFeedNotFound) {
		t.Fatal(e)
	}
	if e := a.store.SetEnabled(999, true); !errors.Is(e, ErrFeedNotFound) {
		t.Fatal(e)
	}
}

func TestDisableCancelsInFlightFetch(t *testing.T) {
	a := testApp(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	done := make(chan struct{})
	go func() { a.poller.fetch(context.Background(), id); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fetch did not start")
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"s": {fmt.Sprintf("feed/%d", id)}, "ac": {"unsubscribe"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disabled fetch was not cancelled")
	}
	if countRows(t, a, "fetches", id) != 0 {
		t.Fatal("cancelled fetch changed history")
	}
	if n := save(t, a, id, &gofeed.Item{GUID: "late"}); n != 0 {
		t.Fatal("late result resurrected disabled entries")
	}
	if e := a.store.DeleteFeed(id); e != nil {
		t.Fatal(e)
	}
	if n := save(t, a, id, &gofeed.Item{GUID: "later"}); n != 0 {
		t.Fatal("late result resurrected deleted subscription")
	}
}

func TestDisabledPersistenceAndAPISubscribe(t *testing.T) {
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	id := addFeed(t, a, "https://example.org/persist")
	save(t, a, id, &gofeed.Item{GUID: "persist"})
	if e = a.store.SetEnabled(id, false); e != nil {
		t.Fatal(e)
	}
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	fs, e := a.store.Feeds()
	if e != nil || len(fs) != 1 || fs[0].Active || fs[0].Count != 1 {
		t.Fatal(fs, e)
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"ac": {"subscribe"}, "s": {"feed/https://example.org/persist"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	fs, e = a.store.Feeds()
	if e != nil || len(fs) != 1 || !fs[0].Active || fs[0].ID != id || fs[0].Count != 1 {
		t.Fatal(fs, e)
	}
}
