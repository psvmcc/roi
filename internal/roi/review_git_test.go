package roi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitOnlyPollsLatestTwentyVersions(t *testing.T) {
	a := testApp(t)
	var count atomic.Int32
	count.Store(625)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("limit") != "20" {
			t.Error("requested more than the latest window", r.URL.RawQuery)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page != 1 {
			t.Error("unnecessary historical page", page)
		}
		rows := []map[string]any{}
		for i := int(count.Load()); i > int(count.Load())-100; i-- {
			rows = append(rows, map[string]any{"tag_name": fmt.Sprintf("v%d", i), "published_at": "2026-10-14T13:57:49Z", "body": "Notes"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	source, _ := parseGitSource(server.URL+"/owner/repo", "forgejo", "releases", false)
	id := storeGit(t, a, source)
	for range 3 {
		a.poller.fetch(t.Context(), id)
	}
	feeds, err := a.store.Feeds()
	if err != nil || feeds[0].Count != 20 || calls.Load() != 3 {
		t.Fatal(feeds, calls.Load(), err)
	}
	count.Store(626)
	a.poller.fetch(t.Context(), id)
	feeds, err = a.store.Feeds()
	if err != nil || feeds[0].Count != 21 {
		t.Fatal("new versions should preserve already stored history", feeds, err)
	}
	var old int
	if err = a.store.DB.QueryRow("SELECT count(*) FROM entries WHERE guid='git:v1'").Scan(&old); err != nil || old != 0 {
		t.Fatal("deep history was downloaded", old, err)
	}
}

func TestGitwebOnlyPollsLatestTwentyVersions(t *testing.T) {
	a := testApp(t)
	var details atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := gitwebQuery(r.URL.RawQuery)
		if q.Get("a") == "tags" {
			fmt.Fprint(w, `<table class="tags">`)
			for i := 45; i > 0; i-- {
				fmt.Fprintf(w, `<tr><td><a href="?p=a.git;a=shortlog;h=refs/tags/v%d">tag</a></td></tr>`, i)
			}
			fmt.Fprint(w, `</table>`)
			return
		}
		details.Add(1)
		version, _ := strconv.Atoi(strings.TrimPrefix(q.Get("h"), "refs/tags/v"))
		if version < 26 {
			t.Error("old Gitweb tag fetched", version)
		}
		fmt.Fprint(w, `<span class="datetime">2026-10-14T13:57:49Z</span><div class="page_body"><a href="https://example.org/changes">Changes</a><br/><code>&lt;safe&gt;</code><script>bad()</script></div>`)
	}))
	defer srv.Close()
	source, _ := parseGitSource(srv.URL+"/?p=a.git", "gitweb", "tags", false)
	id := storeGit(t, a, source)
	for range 3 {
		a.poller.fetch(t.Context(), id)
	}
	feeds, err := a.store.Feeds()
	if err != nil || feeds[0].Count != 20 || details.Load() != 20 {
		t.Fatal(feeds, details.Load(), err)
	}
	var body string
	if err = a.store.DB.QueryRow("SELECT content FROM entries WHERE feed_id=? LIMIT 1", id).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `href="https://example.org/changes"`) || !strings.Contains(body, "<code>&lt;safe&gt;</code>") || strings.Contains(body, "bad()") {
		t.Fatal(body)
	}
}

func TestGitHubTagObjectAndRendererChangesPreserveState(t *testing.T) {
	a := testApp(t)
	var current atomic.Value
	current.Store("old")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags") {
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"name": "v1", "commit": map[string]string{"sha": current.Load().(string)}}})
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"message": "Notes " + current.Load().(string), "committer": map[string]string{"date": "2026-10-14T13:57:49Z"}}})
	}))
	defer srv.Close()
	s, _ := parseGitSource(srv.URL+"/owner/repo", "github", "tags", false)
	id := storeGit(t, a, s)
	a.poller.fetch(t.Context(), id)
	var entryID int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=?", id).Scan(&entryID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE id=?", entryID)
	a.poller.fetch(t.Context(), id)
	if calls.Load() != 1 {
		t.Fatal("unchanged commit reloaded", calls.Load())
	}
	current.Store("new")
	a.poller.fetch(t.Context(), id)
	var gotID int64
	var body string
	var read, starred bool
	if err := a.store.DB.QueryRow("SELECT id,content,is_read,starred FROM entries WHERE feed_id=?", id).Scan(&gotID, &body, &read, &starred); err != nil || gotID != entryID || !read || !starred || !strings.Contains(body, "Notes new") {
		t.Fatal(gotID, body, read, starred, err)
	}
	mustExec(t, a, "UPDATE entries SET content='Broken',render_version=0 WHERE id=?", entryID)
	a.poller.fetch(t.Context(), id)
	if err := a.store.DB.QueryRow("SELECT content FROM entries WHERE id=?", entryID).Scan(&body); err != nil || strings.Contains(body, "Broken") || calls.Load() != 3 {
		t.Fatal(body, calls.Load(), err)
	}
	token := approvedToken(t, a)
	if w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"i": {fmt.Sprint(entryID)}, "a": {statePrefix + "starred"}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestGitHTTPConditionalCacheAndCredentialIsolation(t *testing.T) {
	a := testApp(t)
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 14 Oct 2026 13:57:49 GMT")
		w.Header().Set("Link", `<https://example.org/?page=2>; rel="next"`)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, "[1]")
	}))
	defer srv.Close()
	a.poller.client = srv.Client()
	for _, token := range []string{"one", "one", "two"} {
		budget := gitBudget{Limit: 1024}
		next := false
		b, err := a.poller.gitGet(t.Context(), srv.URL, token, &budget, &next)
		if err != nil || string(b) != "[1]" || budget.Received != 3 || !next {
			t.Fatal(string(b), budget, next, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
	var cached int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM git_http_cache").Scan(&cached); err != nil || cached != 2 {
		t.Fatal(cached, err)
	}
	budget := gitBudget{Limit: 2}
	if _, err := a.poller.gitGet(t.Context(), srv.URL, "one", &budget); err == nil {
		t.Fatal("cached bytes bypassed budget")
	}
	mustExec(t, a, "UPDATE git_http_cache SET checked=0")
	budget = gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), srv.URL, "one", &budget); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "CREATE TRIGGER fail_cache BEFORE INSERT ON git_http_cache BEGIN SELECT RAISE(ABORT,'cache unavailable'); END")
	budget = gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), srv.URL, "three", &budget); err == nil {
		t.Fatal("storage error ignored")
	}
	mustExec(t, a, "DROP TRIGGER fail_cache")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credential request reached plain HTTP server") }))
	defer plain.Close()
	budget = gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), plain.URL, "secret", &budget); err == nil {
		t.Fatal("HTTP credentials allowed")
	}
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotModified) }))
	defer malformed.Close()
	budget = gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), malformed.URL, "", &budget); err == nil {
		t.Fatal("304 without cached response accepted")
	}
}

func TestGitwebWrappedHTTPErrorKeepsStatusAndRetry(t *testing.T) {
	a := testApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := gitwebQuery(r.URL.RawQuery)
		switch q.Get("a") {
		case "tags":
			fmt.Fprint(w, `<table class="tags"><tr><td><a href="?p=a.git;a=tag;h=hash">tag</a><a href="?p=a.git;a=shortlog;h=refs/tags/v1">log</a></td></tr></table>`)
		case "tag":
			fmt.Fprint(w, `<span class="datetime">2026-10-14T13:57:49Z</span><div class="page_body">Notes</div>`)
		case "commit":
			w.Header().Set("Retry-After", "120")
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	source, _ := parseGitSource(srv.URL+"/?p=a.git", "gitweb", "tags", false)
	id := storeGit(t, a, source)
	a.poller.fetch(t.Context(), id)
	var message string
	var status int
	if err := a.store.DB.QueryRow("SELECT error,status FROM fetches WHERE feed_id=?", id).Scan(&message, &status); err != nil || status != http.StatusServiceUnavailable || !strings.HasPrefix(message, "HTTP:") {
		t.Fatal(status, message, err)
	}
}
