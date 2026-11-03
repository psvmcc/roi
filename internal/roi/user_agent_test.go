package roi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestRSSUserAgentRequestsAndCacheReset(t *testing.T) {
	type observed struct{ Agent, ETag, Modified string }
	requests := make(chan observed, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observed{r.UserAgent(), r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/feed", http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"old"`)
		w.Header().Set("Last-Modified", "Wed, 14 Oct 2009 16:12:40 GMT")
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><title>RSS</title><link>https://example.org</link><description>Test</description></channel></rss>`))
	}))
	defer server.Close()
	a := testApp(t)
	id := addFeed(t, a, server.URL+"/redirect")
	for _, agent := range []string{"", "CustomReader/2.0 (test)", ""} {
		if len(requests) > 0 {
			t.Fatal("unexpected pending request")
		}
		// Each save deliberately invalidates cached validators.
		if err := a.store.SetFeedUserAgent(id, agent); err != nil {
			t.Fatal(err)
		}
		a.poller.fetch(context.Background(), id)
		want := agent
		if want == "" {
			want = defaultUserAgent
		}
		for range 2 {
			select {
			case got := <-requests:
				if got.Agent != want || got.ETag != "" || got.Modified != "" {
					t.Fatal(got, want)
				}
			default:
				t.Fatal("missing request")
			}
		}
		feeds, err := a.store.Feeds()
		if err != nil || feeds[0].UserAgent != agent {
			t.Fatal(feeds, err)
		}
	}
}

func TestRSSUserAgentAdminValidationAndState(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "id": {strconv.FormatInt(id, 10)}, "user_agent": {"Custom/2 (test)"}}
	if w := asyncAdmin(a, nil, "POST", "/rss-settings", values); w.Code != 401 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", "wrong")
	if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 403 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", csrf)
	for _, bad := range []string{strings.Repeat("a", 513), "Injected\r\nX-Test: true", "tab\tvalue", "nul\x00", "nonascii-é", "\x7f"} {
		values.Set("user_agent", bad)
		if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 400 {
			t.Fatal(w.Code, bad)
		}
	}
	for _, badID := range []string{"", "wrong", "-1", "0", "999999"} {
		values.Set("id", badID)
		values.Set("user_agent", "valid")
		if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 400 {
			t.Fatal(w.Code, badID)
		}
	}
	source, _ := parseGitSource("https://github.com/team/repo", "github", "tags", false)
	gitID := storeGit(t, a, source)
	values.Set("id", strconv.FormatInt(gitID, 10))
	if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 400 {
		t.Fatal(w.Code)
	}
	values.Set("id", strconv.FormatInt(id, 10))
	values.Set("user_agent", "  Custom/2 (test)  ")
	mustExec(t, a, "UPDATE feeds SET paused=1,etag='old',modified='old',next_fetch=999999 WHERE id=?", id)
	if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var agent, etag, modified string
	var paused, active bool
	var next int64
	if err := a.store.DB.QueryRow("SELECT user_agent,etag,modified,paused,active,next_fetch FROM feeds WHERE id=?", id).Scan(&agent, &etag, &modified, &paused, &active, &next); err != nil || agent != "Custom/2 (test)" || etag != "" || modified != "" || !paused || !active || next != 0 {
		t.Fatal(agent, etag, modified, paused, active, next, err)
	}
	if err := a.store.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	values.Set("user_agent", "")
	if w := asyncAdmin(a, cookie, "POST", "/rss-settings", values); w.Code != 200 {
		t.Fatal(w.Code)
	}
	feeds, _ := a.store.Feeds()
	for _, f := range feeds {
		if f.ID == id && (f.Active || f.UserAgent != "") {
			t.Fatal(f)
		}
	}
	a.Close()
	if err := a.store.SetFeedUserAgent(id, "valid"); err == nil {
		t.Fatal("ignored database error")
	}
}

func TestRSSUserAgentMigrationAndPersistence(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	id := addFeed(t, a, "https://example.org/rss")
	mustExec(t, a, "ALTER TABLE feeds DROP COLUMN user_agent; ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=8")
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 1 || feeds[0].ID != id || feeds[0].UserAgent != "" {
		t.Fatal(feeds, err)
	}
	if err := a.store.SetFeedUserAgent(id, "Persistent/1"); err != nil {
		t.Fatal(err)
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	feeds, err = a.store.Feeds()
	if err != nil || feeds[0].UserAgent != "Persistent/1" {
		t.Fatal(feeds, err)
	}
}

func TestROIUserAgentRoundTripAndValidation(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	if err := a.store.SetFeedUserAgent(id, `Custom/1 ("quoted")`); err != nil {
		t.Fatal(err)
	}
	var data strings.Builder
	if err := a.store.ExportROI(&data); err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err := other.store.ImportROI(strings.NewReader(data.String())); err != nil {
		t.Fatal(err)
	}
	feeds, err := other.store.Feeds()
	if err != nil || feeds[0].UserAgent != `Custom/1 ("quoted")` {
		t.Fatal(feeds, err)
	}
	bad := "format: roi\nversion: 1\nsources:\n  - type: rss\n    url: https://example.org/new\n    user_agent: \"bad\\r\\nInjected: true\"\n"
	if err := other.store.ImportROI(strings.NewReader(bad)); err == nil {
		t.Fatal("unsafe imported header accepted")
	}
	bad = "format: roi\nversion: 1\nsources:\n  - type: git\n    repository: https://github.com/team/repo\n    service: github\n    user_agent: Custom/1\n"
	if err := other.store.ImportROI(strings.NewReader(bad)); err == nil {
		t.Fatal("Git override accepted")
	}
	feeds, _ = other.store.Feeds()
	if len(feeds) != 1 {
		t.Fatal("failed import changed subscriptions")
	}
}
