package roi

import (
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func githubTestCommit(name, oid, date, message string) githubTagRef {
	ref := githubTagRef{Name: name, Target: &githubGitObject{Type: "Commit", OID: oid, Date: date, Message: message}}
	ref.Target.Author.Name = "Author"
	return ref
}
func githubTestPage(r *http.Request, refs []githubTagRef, next bool, cursor string) *http.Response {
	if refs == nil {
		refs = []githubTagRef{}
	}
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"refs": map[string]any{"nodes": refs, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}}, "rateLimit": map[string]any{"remaining": 1000, "resetAt": "2099-01-01T00:00:00Z"}}})
	return gitResponse(r, http.StatusOK, string(body))
}
func githubTestSource(t *testing.T) (*App, GitSource) {
	t.Helper()
	a := testApp(t)
	a.poller.config.Git.Tokens = map[string]string{"github.com": "graphql-test-token"}
	s, err := parseGitSource("https://github.com/owner/repo", "github", "tags", false)
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}
func TestGitHubGraphQLDateOrderFilteringAndCursor(t *testing.T) {
	a, s := githubTestSource(t)
	s.TagFilterMode = "regex"
	s.TagFilter = stableVersionRegex
	calls := 0
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.String() != "https://api.github.com/graphql" || r.Header.Get("Authorization") != "Bearer graphql-test-token" || r.Header.Get("Content-Type") != "application/json" || r.UserAgent() != defaultUserAgent {
			t.Error("invalid GraphQL request", r.Method, r.URL)
		}
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Error("POST must not use GET validators")
		}
		var req struct {
			Query     string
			Variables struct {
				Owner, Name string
				First       int
				After       *string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(req.Query, "TAG_COMMIT_DATE") || !strings.Contains(req.Query, "direction: DESC") || req.Variables.Owner != "owner" || req.Variables.Name != "repo" || req.Variables.First != 100 {
			t.Fatal(req)
		}
		if calls == 1 {
			if req.Variables.After != nil {
				t.Fatal(req)
			}
			refs := []githubTagRef{}
			for i := range 100 {
				refs = append(refs, githubTestCommit(fmt.Sprintf("v99.0.0-dev.%d", i), "dev", "2026-11-15T00:00:00Z", "ignored"))
			}
			return githubTestPage(r, refs, true, "cursor-1"), nil
		}
		if calls != 2 || req.Variables.After == nil || *req.Variables.After != "cursor-1" {
			t.Fatal(calls, req)
		}
		refs := []githubTagRef{githubTestCommit("api/v1.2.3", "prefix", "2026-11-14T00:00:00Z", "ignored"), githubTestCommit("v1.2.3-lalala", "suffix", "2026-11-14T00:00:00Z", "ignored")}
		for i := range 30 {
			refs = append(refs, githubTestCommit(fmt.Sprintf("v1.2.%d", i), "sha"+fmt.Sprint(i), time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i)*time.Hour).Format(time.RFC3339), "**Notes**"))
		}
		return githubTestPage(r, refs, true, "cursor-2"), nil
	})
	feed, err := a.poller.fetchGit(t.Context(), s, 1)
	if err != nil || len(feed.Items) != 20 || calls != 2 {
		t.Fatal(feed, calls, err)
	}
	if feed.Items[0].GUID != "git:v1.2.0" || !strings.Contains(feed.Items[0].Content, "<strong>Notes</strong>") || feed.Items[19].GUID != "git:v1.2.19" {
		t.Fatal(feed.Items)
	}
}
func TestGitHubGraphQLPageBoundAndEmptyRepository(t *testing.T) {
	for _, empty := range []bool{false, true} {
		a, s := githubTestSource(t)
		calls := 0
		a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			if empty {
				return githubTestPage(r, nil, false, ""), nil
			}
			return githubTestPage(r, []githubTagRef{githubTestCommit("v1-beta1", "sha", "2026-01-01T00:00:00Z", "")}, true, fmt.Sprint(calls)), nil
		})
		feed, err := a.poller.fetchGit(t.Context(), s, 1)
		want := gitMaxPages
		if empty {
			want = 1
		}
		if err != nil || len(feed.Items) != 0 || calls != want {
			t.Fatal(feed, calls, err)
		}
	}
}
func TestGitHubGraphQLAnnotatedTagsAndStableState(t *testing.T) {
	a, s := githubTestSource(t)
	calls := 0
	message := "**Tag notes**"
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		commit := githubTestCommit("v1.0.0", "commit-sha", "2024-01-01T00:00:00Z", "Commit notes")
		tag := &githubGitObject{Type: "Tag", OID: "tag-sha", Message: message, Target: commit.Target}
		tag.Tagger.Name = "Tag author"
		return githubTestPage(r, []githubTagRef{{Name: commit.Name, Target: tag}}, false, ""), nil
	})
	id := storeGit(t, a, s)
	a.poller.fetch(t.Context(), id)
	var entryID int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=?", id).Scan(&entryID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE id=?", entryID)
	message = "Updated **tag notes**"
	a.poller.fetch(t.Context(), id)
	var sameID, published int64
	var read, starred bool
	var body, author string
	if err := a.store.DB.QueryRow("SELECT id,published,is_read,starred,content,author FROM entries WHERE feed_id=?", id).Scan(&sameID, &published, &read, &starred, &body, &author); err != nil || sameID != entryID || published != 1704067200 || !read || !starred || !strings.Contains(body, "Updated <strong>tag notes</strong>") || author != "Tag author" || calls != 2 {
		t.Fatal(sameID, published, read, starred, body, author, calls, err)
	}
}
func TestGitHubGraphQLErrorsAndCursors(t *testing.T) {
	for _, body := range []string{
		"{", "null", `{"data":null}`, `{"data":{"repository":null}}`,
		`{"errors":[{"type":"FORBIDDEN","message":"secret must not be echoed"}],"data":{"repository":null}}`,
		`{"data":{"repository":{"refs":{"nodes":[]}}}}`,
		`{"data":{"repository":{"refs":{"nodes":null,"pageInfo":{"hasNextPage":false}}}}}`,
		`{"data":{"repository":{"refs":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			a, s := githubTestSource(t)
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) { return gitResponse(r, http.StatusOK, body), nil })
			if _, err := a.poller.fetchGit(t.Context(), s, 1); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
	for _, loop := range []bool{false, true} {
		a, s := githubTestSource(t)
		calls := 0
		a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			cursor := "same"
			if loop && calls == 2 {
				cursor = "other"
			}
			return githubTestPage(r, nil, true, cursor), nil
		})
		if _, err := a.poller.fetchGit(t.Context(), s, 1); err == nil || calls > 3 {
			t.Fatal(calls, err)
		}
	}
	a, s := githubTestSource(t)
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		refs := []githubTagRef{}
		for range 101 {
			refs = append(refs, githubTestCommit("v1", "sha", "2024-01-01T00:00:00Z", ""))
		}
		return githubTestPage(r, refs, false, ""), nil
	})
	if _, err := a.poller.fetchGit(t.Context(), s, 1); err == nil {
		t.Fatal("oversized page accepted")
	}
}
func TestGitHubGraphQLQuotaErrorsAndNoSilentFallback(t *testing.T) {
	for _, bodyError := range []bool{false, true} {
		a, s := githubTestSource(t)
		calls := 0
		a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			body := `{"errors":[{"type":"RATE_LIMITED"}]}`
			if bodyError {
				body = `{"errors":[{"extensions":{"code":"RATE_LIMITED"}}],"data":{"rateLimit":{"remaining":0,"resetAt":"2099-01-01T00:00:00Z"}}}`
			}
			response := gitResponse(r, http.StatusOK, body)
			if !bodyError {
				response.Header.Set("X-RateLimit-Remaining", "0")
				response.Header.Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
			}
			return response, nil
		})
		_, err := a.poller.fetchGit(t.Context(), s, 1)
		var status *gitHTTPError
		if !errors.As(err, &status) || status.Status != http.StatusTooManyRequests || status.Retry <= 0 || calls != 1 {
			t.Fatal(calls, err)
		}
		if _, err = a.poller.fetchGit(t.Context(), s, 1); err == nil || calls != 1 {
			t.Fatal("cooldown bypassed", calls, err)
		}
	}
	a, s := githubTestSource(t)
	calls := 0
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		return gitResponse(r, http.StatusForbidden, "{}"), nil
	})
	if _, err := a.poller.fetchGit(t.Context(), s, 1); err == nil || calls != 1 {
		t.Fatal("REST fallback after GraphQL failure", calls, err)
	}
}
func TestGitHubGraphQLTargetValidationAndEnterprise(t *testing.T) {
	for _, object := range []*githubGitObject{nil, {Type: "Tree"}, {Type: "Commit", OID: "sha"}, {Type: "Commit", Date: "2024-01-01T00:00:00Z"}} {
		if _, err := githubTagRelease(githubTagRef{Name: "v1", Target: object}); err == nil {
			t.Fatal(object)
		}
	}
	target := githubTestCommit("v1", "sha", "2024-01-01T00:00:00Z", "Message").Target
	for range 4 {
		target = &githubGitObject{Type: "Tag", Target: target}
	}
	if _, err := githubTagRelease(githubTagRef{Name: "v1", Target: target}); err != nil {
		t.Fatal(err)
	}
	target = &githubGitObject{Type: "Tag", Target: target}
	if _, err := githubTagRelease(githubTagRef{Name: "v1", Target: target}); err == nil {
		t.Fatal("unbounded tag chain")
	}
	a, s := githubTestSource(t)
	s.URL = "https://git.example/owner/repo"
	a.poller.config.Git.Tokens = map[string]string{"git.example": "enterprise-token"}
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://git.example/api/graphql" || r.Header.Get("Authorization") != "Bearer enterprise-token" {
			t.Fatal(r.URL)
		}
		return githubTestPage(r, nil, false, ""), nil
	})
	if _, err := a.poller.fetchGit(t.Context(), s, 1); err != nil {
		t.Fatal(err)
	}
	for _, first := range []int{0, 101} {
		if _, err := a.poller.githubTagsPage(t.Context(), s, "", first, &gitBudget{Limit: 1024}); err == nil {
			t.Fatal("invalid page size")
		}
	}
	savedRepo := s.Repo
	s.Repo = "invalid"
	if _, err := a.poller.githubTagsPage(t.Context(), s, "", 1, &gitBudget{Limit: 1024}); err == nil {
		t.Fatal("invalid repository")
	}
	s.Repo = savedRepo
	delete(a.poller.config.Git.Tokens, "git.example")
	if _, err := a.poller.githubTagsPage(t.Context(), s, "", 1, &gitBudget{Limit: 1024}); err == nil {
		t.Fatal("missing token")
	}
	if _, err := a.poller.githubTagsPage(t.Context(), s, "", 0, &gitBudget{Limit: 1024}); err == nil {
		t.Fatal("invalid page size")
	}
}

func TestGitHubGraphQLPostLimitsCompressionAndRedirects(t *testing.T) {
	a, s := githubTestSource(t)
	calls := 0
	var firstPayload string
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			firstPayload = string(payload)
		} else if string(payload) != firstPayload {
			t.Fatal("compression retry changed query")
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Fatal("GraphQL used an ETag cache")
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			response := gitResponse(r, http.StatusOK, "")
			response.Uncompressed = true
			response.Body = io.NopCloser(compressionReadFailure{err: flate.CorruptInputError(1)})
			return response, nil
		}
		response := githubTestPage(r, nil, false, "")
		response.Header.Set("ETag", "must-not-cache")
		return response, nil
	})
	feed, err := a.poller.fetchGit(t.Context(), s, 1)
	if err != nil || len(feed.Items) != 0 || calls != 2 {
		t.Fatal(feed, calls, err)
	}
	var cached int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM git_http_cache").Scan(&cached); err != nil || cached != 0 {
		t.Fatal(cached, err)
	}
	a.poller.config.Poll.MaxBytes = 10
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		return gitResponse(r, http.StatusOK, strings.Repeat("x", 100)), nil
	})
	_, err = a.poller.fetchGit(t.Context(), s, 1)
	var size *gitSizeError
	if !errors.As(err, &size) || strings.Contains(err.Error(), "graphql-test-token") {
		t.Fatal(err)
	}
	a.poller.config.Poll.MaxBytes = 1024
	var redirects atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		w.Header().Set("Location", "/other")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	a.poller.client = server.Client()
	if _, err = a.poller.gitPost(t.Context(), server.URL+"/graphql", "test-token", []byte("{}"), &gitBudget{Limit: 1024}); err == nil || redirects.Load() != 1 {
		t.Fatal(redirects.Load(), err)
	}
	if _, err = a.poller.gitPost(t.Context(), "http://example.org/graphql", "test-token", []byte("{}"), &gitBudget{Limit: 1024}); err == nil {
		t.Fatal("credentials over HTTP")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err = a.poller.gitPost(ctx, "https://example.org/graphql", "test-token", nil, &gitBudget{Limit: 1024}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGitHubGraphQLAdminOrderAndNoTokenCompatibility(t *testing.T) {
	a, s := githubTestSource(t)
	id := storeGit(t, a, s)
	cookie, _ := adminSession(t, a)
	page := adminRequest(a, cookie, "GET", "/feeds", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Commit date order") || strings.Contains(page.Body.String(), "graphql-test-token") {
		t.Fatal(page.Code)
	}
	delete(a.poller.config.Git.Tokens, "github.com")
	page = adminRequest(a, cookie, "GET", "/feeds", nil)
	if !strings.Contains(page.Body.String(), "API default order (no token)") {
		t.Fatal("missing legacy ordering label")
	}
	calls := 0
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/tags") || r.Header.Get("Authorization") != "" {
			t.Fatal("unauthenticated source must remain REST", r.Method, r.URL)
		}
		return gitResponse(r, http.StatusOK, "[{\"name\":\"v1.0.0\",\"commit\":{\"sha\":\"sha\",\"created\":\"2024-01-01T00:00:00Z\"}}]"), nil
	})
	a.poller.fetch(t.Context(), id)
	feeds, _ := a.store.Feeds()
	if calls != 1 || feeds[0].Count != 1 || feeds[0].Error != "" {
		t.Fatal(feeds, calls)
	}
}
