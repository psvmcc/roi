package roi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func fixtureGit(t *testing.T, provider string) (*App, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != defaultUserAgent {
			t.Errorf("unexpected Git User-Agent: %q", r.UserAgent())
		}
		if r.URL.Path == "/" {
			fmt.Fprintf(w, `<html><head><meta name="application-name" content="%s"><link rel="icon" href="assets/favicon.svg"></head></html>`, provider)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/releases") {
			rows := []map[string]any{
				{"id": 3, "name": "Preview", "tag_name": "v3.0.0-rc1", "prerelease": true, "body": "Preview notes", "published_at": "2024-03-03T12:00:00Z", "released_at": "2024-03-03T12:00:00Z"},
				{"id": 2, "name": "Stable v2", "tag_name": "v2.0.0", "body": "# Highlights\n\n**Fast** and `safe`.\n\n- One\n- Two\n\n[Changes](../../compare/v1...v2)\n\n<script>bad()</script>", "description": "## Stable GitLab notes\n\n**Great**", "published_at": "2024-02-02T12:00:00Z", "released_at": "2024-02-02T12:00:00Z", "author": map[string]string{"login": "maintainer"}},
				{"id": 1, "tag_name": "v1.0.0", "published_at": "2024-01-01T12:00:00Z", "released_at": "2024-01-01T12:00:00Z"},
				{"id": 4, "name": "Draft", "tag_name": "v4.0.0", "draft": true, "published_at": "2024-04-04T12:00:00Z", "released_at": "2024-04-04T12:00:00Z"},
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tags") {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "v3.0.0-beta1", "commit": map[string]string{"sha": "beta", "created": "2024-03-03T12:00:00Z", "committed_date": "2024-03-03T12:00:00Z"}},
				{"name": "v2.0.0", "message": "Tag message", "commit": map[string]string{"sha": "stable", "created": "2024-02-02T12:00:00Z", "committed_date": "2024-02-02T12:00:00Z"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return testApp(t), server
}
func storeGit(t *testing.T, a *App, s GitSource) int64 {
	t.Helper()
	if err := a.store.SaveGitSource(0, s); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := a.store.DB.QueryRow("SELECT id FROM feeds WHERE url=?", s.URL).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestGitServicesReleasesTagsAndMarkdown(t *testing.T) {
	for _, provider := range []string{"github", "forgejo", "gitea", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			a, server := fixtureGit(t, provider)
			repo := "owner/project"
			if provider == "gitlab" {
				repo = "group/subgroup/project"
			}
			source, err := parseGitSource(server.URL+"/"+repo, "auto", "releases", false)
			if err != nil {
				t.Fatal(err)
			}
			source, err = a.poller.detectGit(t.Context(), source)
			if err != nil || source.Provider != provider || source.Icon != server.URL+"/assets/favicon.svg" {
				t.Fatal(source, err)
			}
			id := storeGit(t, a, source)
			a.poller.fetch(t.Context(), id)
			feeds, err := a.store.Feeds()
			if err != nil || len(feeds) != 1 || feeds[0].Count != 2 || feeds[0].Error != "" || feeds[0].Title != source.title() {
				t.Fatal(feeds, err)
			}
			if len(feeds[0].Folders) != 1 || feeds[0].Folders[0] != gitFolder {
				t.Fatal(feeds[0])
			}
			token := approvedToken(t, a)
			w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "GIT_Releases") || !strings.Contains(w.Body.String(), "iconUrl") {
				t.Fatal(w.Body.String())
			}
			w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", nil)
			var stream struct {
				Items []struct {
					Published int64
					Summary   struct{ Content string }
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &stream); err != nil || len(stream.Items) != 2 || stream.Items[0].Published != 1706875200 || strings.Contains(stream.Items[0].Summary.Content, "<script>") {
				t.Fatal(w.Body.String(), err)
			}
			source.Preview = true
			if err := a.store.SaveGitSource(id, source); err != nil {
				t.Fatal(err)
			}
			a.poller.fetch(t.Context(), id)
			var count int
			a.store.DB.QueryRow("SELECT count(*) FROM entries").Scan(&count)
			if count != 3 {
				t.Fatal("preview/draft filtering incorrect", count)
			}
			source.Preview = false
			if err := a.store.SaveGitSource(id, source); err != nil {
				t.Fatal(err)
			}
			w = apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", nil)
			var refs struct {
				Refs []any `json:"itemRefs"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &refs)
			if len(refs.Refs) != 2 {
				t.Fatal("cached preview leaked", w.Body.String())
			}
			var previewID int64
			a.store.DB.QueryRow("SELECT id FROM entries WHERE git_preview=1").Scan(&previewID)
			w = apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {fmt.Sprint(previewID)}})
			if strings.Contains(w.Body.String(), "Preview notes") {
				t.Fatal("direct-ID preview leak")
			}
			w = apiReq(a, token, "GET", "/reader/api/0/unread-count", nil)
			if w.Code != 200 || strings.Contains(w.Body.String(), `"count":3`) {
				t.Fatal(w.Body.String())
			}
			if provider != "github" {
				source.Mode = "tags"
				if err := a.store.SaveGitSource(id, source); err != nil {
					t.Fatal(err)
				}
				a.poller.fetch(t.Context(), id)
				var content string
				a.store.DB.QueryRow("SELECT content FROM entries WHERE guid='git:v2.0.0'").Scan(&content)
				if !strings.Contains(content, "Tag message") {
					t.Fatal(content)
				}
			}
		})
	}
}
func TestGitFolderAPICannotCreateMoveRenameOrReactivate(t *testing.T) {
	a, server := fixtureGit(t, "gitea")
	s, _ := parseGitSource(server.URL+"/owner/project", "gitea", "releases", false)
	id := storeGit(t, a, s)
	normal := addFeed(t, a, "https://example.org/rss")
	token := approvedToken(t, a)
	for _, tc := range []struct {
		route  string
		values url.Values
	}{
		{"subscription/quickadd", url.Values{"quickadd": {"https://example.org/new"}, "a": {"user/1/label/GIT_Releases"}}},
		{"subscription/edit", url.Values{"s": {"feed/" + fmt.Sprint(normal)}, "ac": {"edit"}, "a": {"user/1/label/GIT_Releases/child"}}},
		{"subscription/edit", url.Values{"s": {"feed/" + fmt.Sprint(id)}, "ac": {"edit"}, "r": {"user/1/label/GIT_Releases"}}},
		{"subscription/quickadd", url.Values{"quickadd": {s.URL}}},
		{"rename-tag", url.Values{"s": {"user/1/label/Other"}, "dest": {"user/1/label/GIT_Releases"}}},
		{"rename-tag", url.Values{"s": {"user/1/label/GIT_Releases"}, "dest": {"user/1/label/Other"}}},
		{"disable-tag", url.Values{"s": {"user/1/label/GIT_Releases"}}},
	} {
		tc.values.Set("T", "x")
		w := apiReq(a, token, "POST", "/reader/api/0/"+tc.route, tc.values)
		if w.Code != 403 {
			t.Fatal(tc.route, w.Code, w.Body.String())
		}
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"ac": {"unsubscribe"}, "s": {"feed/" + fmt.Sprint(id)}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"ac": {"subscribe"}, "s": {"feed/" + s.URL}}); w.Code != 403 {
		t.Fatal(w.Body.String())
	}
	if _, err := a.store.DB.Exec("INSERT INTO feed_folders VALUES(?,?)", normal, gitFolder); err == nil {
		t.Fatal("reserved folder accepts RSS")
	}
}
func TestGitAdminAddEditCSRFOPMLAndStaleResults(t *testing.T) {
	a, server := fixtureGit(t, "forgejo")
	cookie, csrf := adminSession(t, a)
	values := url.Values{"id": {"0"}, "repository": {server.URL + "/owner/project"}, "provider": {"auto"}, "mode": {"releases"}, "csrf": {csrf}}
	if w := asyncAdmin(a, nil, "POST", "/git-source", values); w.Code != 401 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", "bad")
	if w := asyncAdmin(a, cookie, "POST", "/git-source", values); w.Code != 403 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", csrf)
	if w := asyncAdmin(a, cookie, "POST", "/git-source", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, _ := a.store.Feeds()
	id := feeds[0].ID
	var export strings.Builder
	if err := a.store.ExportROI(&export); err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err := other.store.ImportROI(strings.NewReader(export.String())); err != nil {
		t.Fatal(export.String(), err)
	}
	copied, _ := other.store.Feeds()
	if len(copied) != 1 || copied[0].GitProvider != "forgejo" {
		t.Fatal(copied)
	}
	// Duplicate OPML entries do not change a previously configured source.
	if err := other.store.ImportROI(strings.NewReader(export.String())); err != nil {
		t.Fatal(err)
	}
	values.Set("id", fmt.Sprint(id))
	values.Set("mode", "tags")
	values.Set("include_preview", "1")
	if w := asyncAdmin(a, cookie, "POST", "/git-source", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, _ = a.store.Feeds()
	if !feeds[0].GitPreview || feeds[0].GitMode != "tags" {
		t.Fatal(feeds[0])
	}
	old := &gofeed.Feed{Items: []*gofeed.Item{{GUID: "stale", Title: "Must not arrive"}}}
	if n, err := a.poller.saveEntriesRevision(t.Context(), id, old, time.Now(), 0); err != nil || n != 0 {
		t.Fatal("stale settings saved", n, err)
	}
	for _, bad := range []url.Values{{"id": {"bad"}}, {"id": {"-1"}}, {"id": {"9999"}}, {"id": {"0"}, "repository": {"invalid"}, "mode": {"tags"}}, {"id": {"0"}, "repository": {server.URL + "/owner/project"}, "provider": {"gitea"}, "mode": {"bad"}}} {
		bad.Set("csrf", csrf)
		if w := asyncAdmin(a, cookie, "POST", "/git-source", bad); w.Code < 400 {
			t.Fatal(w.Code)
		}
	}
}

func TestGitParsingPreviewAndSafeMarkdown(t *testing.T) {
	for _, tag := range []string{"v1.0.0-rc1", "v2.0beta2", "v1.0.0-preview.2", "nightly", "release-dev", "test-1", "v1.2.3-alpha"} {
		if !isPreviewTag(tag) {
			t.Fatal(tag)
		}
	}
	for _, tag := range []string{"v1.0.0", "release-2026.11", "latest", "contest-1", "v1.2.3+build"} {
		if isPreviewTag(tag) {
			t.Fatal(tag)
		}
	}
	for _, tc := range []struct{ raw, provider, mode string }{{"https://github.com/org/repo", "github", "tags"}, {"https://gitlab.com/group/sub/repo.git/", "gitlab", "releases"}, {"https://forge.example/a/b", "forgejo", "tags"}} {
		if _, err := parseGitSource(tc.raw, tc.provider, tc.mode, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ raw, provider, mode string }{{"bad", "github", "tags"}, {"https://github.com/org", "github", "tags"}, {"https://github.com/org/a/b", "github", "tags"}, {"https://gitlab.com/a/b?token=x", "gitlab", "tags"}, {"https://host/a/%62", "auto", "tags"}, {"https://host/a/../b", "auto", "tags"}, {"https://host/a/b", "bad", "tags"}, {"https://host/a/b", "gitea", "bad"}, {strings.Repeat("x", 4097), "auto", "tags"}} {
		if _, err := parseGitSource(tc.raw, tc.provider, tc.mode, false); err == nil {
			t.Fatal(tc)
		}
	}
	for _, body := range []string{"", "# Title\n\n**bold** _em_ ~~strike~~\n\n| A | B |\n|---|---|\n| 1 | 2 |\n\n```go\n<script>\n```\n\n[relative](../changes) [bad](javascript:alert(1))\n<script>alert(1)</script>", strings.Repeat("x", 100005), string([]byte{255})} {
		out, err := gitMarkdown(body, "https://example.org/org/repo/releases/v1")
		if err != nil || strings.Contains(out, "<script>") || strings.Contains(out, `href="javascript:`) {
			t.Fatal(out, err)
		}
	}
	if gitDate("invalid") != nil {
		t.Fatal("invalid date accepted")
	}
	for _, body := range []string{"null", "{}", "[", `[{},`, strings.Repeat("x", 100), `[] []`, "[" + strings.Repeat("{},", 100) + "{}]"} {
		if _, err := decodeGitList([]byte(body)); err == nil {
			t.Fatal("invalid list accepted", body)
		}
	}
}

func TestGitHTTPBudgetsRateLimitsAndRedirectSafety(t *testing.T) {
	a := testApp(t)
	var stolen atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			stolen.Store(true)
		}
		fmt.Fprint(w, "[]")
	}))
	defer other.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/rate":
			w.Header().Set("Retry-After", "30")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
			w.WriteHeader(429)
		case "/date":
			w.Header().Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(503)
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", 2048))
		default:
			fmt.Fprint(w, "[]")
		}
	}))
	defer server.Close()
	a.poller.client = server.Client()
	for _, path := range []string{"/redirect", "/rate", "/date", "/large"} {
		a.poller.mu.Lock()
		a.poller.hosts = map[string]*hostState{}
		a.poller.mu.Unlock()
		budget := gitBudget{Limit: 1024}
		_, err := a.poller.gitGet(t.Context(), server.URL+path, "private-token", &budget)
		if err == nil {
			t.Fatal(path)
		}
		if e, ok := err.(*gitHTTPError); ok && e.Retry <= 0 {
			t.Fatal(e)
		}
	}
	if stolen.Load() {
		t.Fatal("token leaked across origins")
	}
	budget := gitBudget{}
	if _, err := a.poller.gitGet(t.Context(), server.URL, "", &budget); err == nil {
		t.Fatal("zero budget accepted")
	}
	budget = gitBudget{Limit: 1024}
	if _, err := a.poller.gitGet(t.Context(), ":bad", "", &budget); err == nil {
		t.Fatal("invalid request accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.poller.gitGet(ctx, server.URL, "", &budget); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

type gitRoundTripper func(*http.Request) (*http.Response, error)

func (f gitRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func gitResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestGitHubTagDatesCachedContentsAndHostScopedToken(t *testing.T) {
	a := testApp(t)
	var commits, pages int
	a.config.Git.Tokens = map[string]string{"github.com": "scoped-token"}
	a.poller.config.Git.Tokens = a.config.Git.Tokens
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			if r.Header.Get("Authorization") != "" {
				t.Error("token sent to website")
			}
			return gitResponse(r, 200, `<link rel="icon" href="https://github.githubassets.com/favicons/favicon.svg">`), nil
		}
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer scoped-token" {
			t.Error("wrong token scope", r.URL)
		}
		if strings.HasSuffix(r.URL.Path, "/tags") {
			pages++
			return gitResponse(r, 200, `[{"name":"v1.0.0","commit":{"sha":"abc123"}}]`), nil
		}
		if strings.HasSuffix(r.URL.Path, "/commits/abc123") {
			commits++
			return gitResponse(r, 200, `{"commit":{"message":"**Commit notes**","author":{"name":"Author","date":"2020-05-06T10:00:00Z"},"committer":{"date":"2020-05-06T12:00:00Z"}}}`), nil
		}
		return gitResponse(r, 404, `{}`), nil
	})
	s, _ := parseGitSource("https://GitHub.com/owner/repo.git", "auto", "tags", false)
	s, err := a.poller.detectGit(t.Context(), s)
	if err != nil || s.Provider != "github" || s.title() != "github.com :: owner/repo" {
		t.Fatal(s, err)
	}
	id := storeGit(t, a, s)
	a.poller.fetch(t.Context(), id)
	a.poller.fetch(t.Context(), id)
	if commits != 1 || pages != 3 {
		t.Fatal("tag commit metadata was not cached", commits, pages)
	}
	var published int64
	var content string
	if err := a.store.DB.QueryRow("SELECT published,content FROM entries WHERE feed_id=?", id).Scan(&published, &content); err != nil || published != 1588766400 || !strings.Contains(content, "<strong>Commit notes</strong>") {
		t.Fatal(published, content, err)
	}
}

func TestGitPaginationPastPreviewsAndProviderPageClamps(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			a := testApp(t)
			var pages int
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
				pages++
				page := r.URL.Query().Get("page")
				count := 100
				if provider == "forgejo" {
					count = 2
				}
				rows := []map[string]any{}
				if page == "1" {
					for i := 0; i < count; i++ {
						rows = append(rows, map[string]any{"tag_name": fmt.Sprintf("v2.0.0-rc%d", i), "prerelease": true, "published_at": "2024-01-02T00:00:00Z"})
					}
				} else {
					for i := 1; i <= 20; i++ {
						rows = append(rows, map[string]any{"tag_name": fmt.Sprintf("v1.0.%d", i), "published_at": "2024-01-01T00:00:00Z"})
					}
				}
				b, _ := json.Marshal(rows)
				resp := gitResponse(r, 200, string(b))
				if page == "1" {
					resp.Header.Set("Link", `<https://forge.example/ignored>; rel="next"`)
				}
				return resp, nil
			})
			s, _ := parseGitSource("https://forge.example/owner/repo", provider, "releases", false)
			feed, err := a.poller.fetchGit(t.Context(), s, 0)
			if err != nil || len(feed.Items) != 20 || pages != 2 {
				t.Fatal(len(feed.Items), pages, err)
			}
		})
	}
}

func TestGitDetectionProbesAndDiscoveryFailures(t *testing.T) {
	for _, provider := range []string{"forgejo", "gitea", "gitlab", "unknown", "bad-json", "bad-path"} {
		t.Run(provider, func(t *testing.T) {
			a := testApp(t)
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
				path := r.URL.Path
				if path == "/" {
					if provider == "bad-path" {
						return gitResponse(r, 200, `<meta name="generator" content="Gitea">`), nil
					}
					return gitResponse(r, 404, ""), nil
				}
				if path == "/api/forgejo/v1/version" && provider == "forgejo" || path == "/api/v1/version" && (provider == "gitea" || provider == "bad-json") || path == "/api/v4/version" && provider == "gitlab" {
					return gitResponse(r, 200, `{"version":"1.0"}`), nil
				}
				if strings.HasSuffix(path, "/releases") {
					if provider == "bad-json" {
						return gitResponse(r, 200, `{}`), nil
					}
					return gitResponse(r, 200, `[]`), nil
				}
				return gitResponse(r, 404, ""), nil
			})
			path := "/owner/repo"
			if provider == "bad-path" {
				path = "/group/sub/repo"
			}
			s, _ := parseGitSource("https://forge.example"+path, "auto", "releases", false)
			detected, err := a.poller.detectGit(t.Context(), s)
			if provider == "unknown" || provider == "bad-json" || provider == "bad-path" {
				if err == nil {
					t.Fatal("invalid discovery succeeded")
				}
			} else if err != nil || detected.Provider != provider {
				t.Fatal(detected, err)
			}
		})
	}
	// Well-known hosts still work when their website cannot be fetched.
	for host, provider := range map[string]string{"gitlab.com": "gitlab", "codeberg.org": "forgejo", "gitea.com": "gitea"} {
		a := testApp(t)
		a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/" {
				return gitResponse(r, 404, ""), nil
			}
			return gitResponse(r, 200, "[]"), nil
		})
		s, _ := parseGitSource("https://"+host+"/owner/repo", "auto", "tags", false)
		detected, err := a.poller.detectGit(t.Context(), s)
		if err != nil || detected.Provider != provider {
			t.Fatal(detected, err)
		}
	}
}

func TestGitPollingFailuresAndUnknownSourceDates(t *testing.T) {
	for _, mode := range []string{"rate", "invalid", "no-date", "invalid-url", "missing-sha", "invalid-commit", "failed-commit", "tag-description"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			s, _ := parseGitSource("https://forge.example/owner/repo", "github", "releases", false)
			if strings.Contains(mode, "commit") || mode == "missing-sha" || mode == "tag-description" {
				s.Mode = "tags"
			}
			id := storeGit(t, a, s)
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
				if mode == "rate" {
					resp := gitResponse(r, 429, "{}")
					resp.Header.Set("Retry-After", "60")
					return resp, nil
				}
				if mode == "invalid" {
					return gitResponse(r, 200, "{}"), nil
				}
				if strings.Contains(r.URL.Path, "/commits/") {
					if mode == "failed-commit" {
						return gitResponse(r, 503, "{}"), nil
					}
					return gitResponse(r, 200, "{"), nil
				}
				if mode == "no-date" {
					return gitResponse(r, 200, `[{"tag_name":"v1"}]`), nil
				}
				if mode == "invalid-url" {
					return gitResponse(r, 200, `[{"tag_name":"v1","published_at":"2024-01-01T00:00:00Z","html_url":"javascript:bad"}]`), nil
				}
				if mode == "missing-sha" {
					return gitResponse(r, 200, `[{"name":"v1"}]`), nil
				}
				if mode == "tag-description" {
					return gitResponse(r, 200, `[{"name":"v1","created_at":"2024-01-01T00:00:00Z","release":{"description":"Tag **release** notes"}}]`), nil
				}
				return gitResponse(r, 200, `[{"name":"v1","commit":{"sha":"abc"}}]`), nil
			})
			a.poller.fetch(t.Context(), id)
			var failures, next, count int64
			var message string
			a.store.DB.QueryRow("SELECT failures,next_fetch,error,(SELECT count(*) FROM entries) FROM feeds WHERE id=?", id).Scan(&failures, &next, &message, &count)
			if mode == "no-date" {
				if count != 0 || failures != 0 {
					t.Fatal("fabricated date", count, failures)
				}
			} else if mode == "tag-description" {
				if count != 1 {
					t.Fatal(message, count)
				}
			} else if failures != 1 || message == "" {
				t.Fatal("poll failure not retained", failures, message)
			}
			if mode == "rate" && next < time.Now().Add(55*time.Second).Unix() {
				t.Fatal("Retry-After not respected", next)
			}
		})
	}
}

func TestGitStoreDuplicateInvalidAndTransactionalFailures(t *testing.T) {
	a := testApp(t)
	s, _ := parseGitSource("https://forge.example/owner/repo", "gitea", "releases", false)
	id := storeGit(t, a, s)
	if err := a.store.SaveGitSource(0, s); err == nil {
		t.Fatal("duplicate source accepted")
	}
	if err := a.store.SaveGitSource(id+1, s); err == nil {
		t.Fatal("unknown source changed")
	}
	auto := s
	auto.Provider = "auto"
	if err := a.store.SaveGitSource(0, auto); err == nil {
		t.Fatal("undetected source saved")
	}
	invalid := s
	invalid.Mode = "bad"
	if err := a.store.SaveGitSource(0, invalid); err == nil {
		t.Fatal("invalid mode saved")
	}
	for _, opml := range []string{`<opml><body><outline text="GIT_Releases"><outline xmlUrl="https://example.org/rss"/></outline></body></opml>`, `<opml><body><outline type="git" xmlUrl="https://example.org/a/b" roiProvider="auto" roiMode="tags"/></body></opml>`, `<opml><body><outline type="git" xmlUrl="invalid" roiProvider="gitea" roiMode="tags"/></body></opml>`} {
		if err := a.store.Import(strings.NewReader(opml)); err == nil {
			t.Fatal("invalid reserved import accepted")
		}
	}
	for _, table := range []string{"feeds", "folders", "feed_folders"} {
		t.Run(table, func(t *testing.T) {
			other := testApp(t)
			trigger := "CREATE TRIGGER reject_insert BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT,'storage failed'); END"
			mustExec(t, other, trigger)
			if err := other.store.SaveGitSource(0, s); err == nil {
				t.Fatal("failed insertion succeeded")
			}
			var count int
			other.store.DB.QueryRow("SELECT count(*) FROM feeds").Scan(&count)
			if count != 0 {
				t.Fatal("partial Git source committed")
			}
		})
	}
	mustExec(t, a, `CREATE TRIGGER reject_update BEFORE UPDATE ON feeds BEGIN SELECT RAISE(ABORT,'storage failed'); END`)
	if err := a.store.SaveGitSource(id, s); err == nil {
		t.Fatal("failed edit succeeded")
	}
	a.Close()
	if err := a.store.SaveGitSource(0, s); err == nil {
		t.Fatal("closed database accepted source")
	}
}
