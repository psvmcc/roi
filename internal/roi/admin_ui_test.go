package roi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func asyncAdmin(a *App, cookie *http.Cookie, method, path string, values url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-ROI-Request", "async")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	return w
}
func TestAdminFilteredFragmentsUnreadFoldersAndErrorPriority(t *testing.T) {
	a := testApp(t)
	healthy := addFeed(t, a, "https://example.org/healthy")
	failed := addFeed(t, a, "https://example.org/failed")
	disabled := addFeed(t, a, "https://example.org/disabled")
	save(t, a, healthy, &gofeed.Item{GUID: "healthy"})
	save(t, a, failed, &gofeed.Item{GUID: "failed"})
	save(t, a, disabled, &gofeed.Item{GUID: "disabled"})
	mustExec(t, a, "UPDATE feeds SET title='A healthy' WHERE id=?", healthy)
	mustExec(t, a, "UPDATE feeds SET title='Z failed',error='HTTP 500',failures=3 WHERE id=?", failed)
	mustExec(t, a, "UPDATE feeds SET title='Disabled record',active=0 WHERE id=?", disabled)
	mustExec(t, a, "INSERT INTO folders VALUES('Engineering/Go')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Engineering/Go')", healthy)
	cookie, _ := adminSession(t, a)
	w := adminRequest(a, cookie, "GET", "/", nil)
	body := w.Body.String()
	table := strings.Split(strings.Split(body, `aria-label="RSS Feeds"`)[1], "</table>")[0]
	if w.Code != 200 || strings.Contains(table, "Disabled record") || strings.Index(table, "Z failed") > strings.Index(table, "A healthy") || !strings.Contains(table, "Engineering/Go") {
		t.Fatal("filter, ordering, or folders incorrect")
	}
	if !strings.Contains(body, `<span>Unread</span><strong>2</strong>`) || !strings.Contains(body, "3 stored") || !strings.Contains(body, "1 in disabled feeds") {
		t.Fatal("visible and stored unread totals disagree")
	}
	if strings.Contains(body, "Subscriptions and sync") {
		t.Fatal("redundant subtitle")
	}
	w = adminRequest(a, cookie, "GET", "/?disabled=1", nil)
	body = w.Body.String()
	table = strings.Split(strings.Split(body, `aria-label="RSS Feeds"`)[1], "</table>")[0]
	if !strings.Contains(table, "Disabled record") || strings.Contains(table, "A healthy") || strings.Contains(table, "Z failed") {
		t.Fatal("disabled-only filter incorrect")
	}
	w = asyncAdmin(a, cookie, "GET", "/?fragment=1&disabled=1", nil)
	body = w.Body.String()
	if w.Code != 200 || strings.Contains(body, "<!doctype") || strings.Contains(body, "<script") {
		t.Fatal("fragment returned full page")
	}
	for _, id := range []string{"summary", "feed-list", "recent-syncs", "security-events", "storage-footer"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatal("missing fragment", id)
		}
	}
}
func TestAdminAsyncMutationsRenameAndSessionExpiry(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	cookie, csrf := adminSession(t, a)
	form := url.Values{"csrf": {csrf}, "id": {fmt.Sprint(id)}, "title": {"  My name  "}}
	for _, path := range []string{"/rename", "/sync", "/toggle", "/disable", "/enable"} {
		w := asyncAdmin(a, cookie, "POST", path, form)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) || w.Header().Get("Location") != "" {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	fs, e := a.store.Feeds()
	if e != nil || fs[0].Title != "My name" {
		t.Fatal(fs, e)
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil)
	if !strings.Contains(w.Body.String(), "My name") {
		t.Fatal("rename missing from Reader API")
	}
	w = adminRequest(a, cookie, "GET", "/export", nil)
	if !strings.Contains(w.Body.String(), "My name") {
		t.Fatal("rename missing from OPML")
	}
	for _, title := range []string{" ", strings.Repeat("a", 513), "line\nbreak", string([]byte{0xff})} {
		form.Set("title", title)
		w = asyncAdmin(a, cookie, "POST", "/rename", form)
		if w.Code != 400 {
			t.Fatal("invalid title accepted", w.Code)
		}
	}
	form.Set("title", "valid")
	form.Set("id", "invalid")
	if w := asyncAdmin(a, cookie, "POST", "/rename", form); w.Code != 400 {
		t.Fatal(w.Code)
	}
	form.Set("id", "999")
	if w := asyncAdmin(a, cookie, "POST", "/rename", form); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, nil, "POST", "/rename", form); w.Code != 401 {
		t.Fatal("async expiry redirected instead of 401")
	}
	if w := asyncAdmin(a, &http.Cookie{Name: "roi_session", Value: "unknown"}, "GET", "/?fragment=1", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = asyncAdmin(a, cookie, "POST", "/logout", url.Values{"csrf": {csrf}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redirect":"/login"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestRenameFeedStorageAndAllAsyncMessages(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	if e := a.store.SetEnabled(id, false); e != nil {
		t.Fatal(e)
	}
	if e := a.store.RenameFeed(id, "Disabled alias"); e != nil {
		t.Fatal(e)
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	if e := a.store.RenameFeed(id, "valid"); e == nil {
		t.Fatal("storage error ignored")
	}
	for _, path := range []string{"/sync", "/disable", "/enable", "/delete", "/rename", "/import", "/toggle", "/revoke-api-tokens", "/unknown"} {
		if adminActionMessage(path) == "" {
			t.Fatal(path)
		}
	}
}
