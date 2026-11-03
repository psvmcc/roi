package roi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitwebURLsAndTagFilters(t *testing.T) {
	for _, raw := range []string{"https://git.haproxy.org/?p=haproxy-3.4.git;a=summary", "https://git.haproxy.org/?a=tags&p=haproxy-3.4.git", "https://git.haproxy.org/?p=haproxy-3.4.git", "https://git.haproxy.org?p=haproxy-3.4.git"} {
		s, err := parseGitSource(raw, "auto", "releases", false)
		if err != nil || s.Provider != "gitweb" || s.Mode != "tags" || s.title() != "git.haproxy.org :: haproxy-3.4" || s.URL != "https://git.haproxy.org/?a=summary&p=haproxy-3.4.git" {
			t.Fatal(s, err)
		}
	}
	for _, raw := range []string{"https://example.org/", "https://example.org/?p=a", "https://example.org/?p=.git", "https://example.org/?p=%FF.git", "https://example.org/?p=a%0Bb.git", "https://example.org/?p=a.git&p=b.git", "https://example.org/?p=../a.git", "https://example.org/?p=a%5Cb.git", "https://example.org/?p=a%20b.git", "https://example.org/?p=%.git", "https://example.org/?p=/a.git", "https://example.org/%61?p=a.git", "https://example.org/?p=" + strings.Repeat("x", 513) + ".git"} {
		if _, err := parseGitSource(raw, "gitweb", "tags", false); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	if _, err := parseGitSource("https://example.org/?p=a.git", "gitweb", "unknown", false); err == nil {
		t.Fatal("invalid mode")
	}
	s, err := parseGitSource("https://example.org/gitweb.cgi?p=group/project.git;a=summary", "gitweb", "tags", false)
	if err != nil || s.Repo != "group/project" || !strings.Contains(s.gitwebURL("tag", "refs/tags/v1"), "/gitweb.cgi?") {
		t.Fatal(s, err)
	}
	for _, p := range []string{strings.Repeat("x", 257), "[abc]", `a\b`, "a\nb", string([]byte{255})} {
		if validateTagFilter(p) == nil {
			t.Fatal(p)
		}
	}
	for _, tc := range []struct {
		p, tag string
		want   bool
	}{{"", "anything", true}, {"v3.4.*", "v3.4.7", true}, {"v3.4.*", "v3.3.7", false}, {"v?.*", "v3.4", true}, {"v?.*", "v33.4", false}, {"v*", "V3", false}, {"release/*", "release/v1", true}, {"v1.2+3", "v1.2+3", true}, {"v1.2+3", "v1x2+3", false}} {
		if err := validateTagFilter(tc.p); err != nil || matchesTag(tc.p, tc.tag) != tc.want {
			t.Fatal(tc, err)
		}
	}
}

func fixtureGitweb(t *testing.T) (*App, *httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials forwarded to Gitweb")
		}
		if r.UserAgent() != defaultUserAgent {
			t.Error("wrong user agent")
		}
		q, err := gitwebQuery(r.URL.RawQuery)
		if err != nil {
			t.Error(err)
		}
		if q.Get("p") != "haproxy-3.4.git" {
			http.NotFound(w, r)
			return
		}
		switch q.Get("a") {
		case "summary":
			fmt.Fprint(w, `<meta name="generator" content="gitweb/1.7"><link rel="shortcut icon" href="static/git.png">`)
		case "tags":
			fmt.Fprint(w, `<table class="tags">`)
			for _, name := range []string{"v3.4.8-rc1", "v3.4.7", "v3.4.6", "v3.3.9", "v3.4.7"} {
				fmt.Fprintf(w, `<tr><td><a href="?p=haproxy-3.4.git;a=shortlog;h=refs/tags/%s">shortlog</a></td>`, name)
				if name != "v3.4.6" {
					fmt.Fprintf(w, `<td><a href="?p=haproxy-3.4.git;a=tag;h=%s">tag</a><a href="?p=haproxy-3.4.git;a=commit;h=commit-%s">commit</a></td>`, name, name)
				}
				fmt.Fprint(w, `</tr>`)
			}
			fmt.Fprint(w, `</table>`)
		case "tag", "commit":
			calls.Add(1)
			if q.Get("a") == "commit" {
				fmt.Fprint(w, `<span class="datetime">2026-10-13T10:00:00Z</span><div class="page_body">## Changes<br/>
<br/>
Changes:<br/>
&nbsp;&nbsp;&nbsp;&nbsp;- Commit fix<br/>
&nbsp;&nbsp;&nbsp;&nbsp;- Another fix</div>`)
				return
			}
			fmt.Fprint(w, `<table class="object_header"><tr><td>author</td><td>Maintainer &lt;test@example.org&gt;</td></tr><tr><td></td><td><span class="datetime">Wed, 14 Oct 2026 13:57:49 +0000</span></td></tr></table><div class="page_body"># Release notes<br/><br/>**Fixed** [details](https://example.org/notes)<br/>-----BEGIN&nbsp;PGP&nbsp;SIGNATURE-----<br/>signature</div>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := testApp(t)
	u, _ := url.Parse(srv.URL)
	a.poller.config.Git.Tokens = map[string]string{u.Host: "must-not-leak"}
	return a, srv, calls
}

func TestGitwebAdminSyncCacheFilterAndRoundTrip(t *testing.T) {
	a, srv, calls := fixtureGitweb(t)
	cookie, csrf := adminSession(t, a)
	form := url.Values{"csrf": {csrf}, "id": {"0"}, "provider": {"auto"}, "repository": {srv.URL + "/gitweb.cgi?p=haproxy-3.4.git;a=summary"}, "mode": {"releases"}, "tag_filter": {"v3.4.*"}}
	w := asyncAdmin(a, cookie, "POST", "/git-source", form)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	fs, err := a.store.Feeds()
	if err != nil || len(fs) != 1 {
		t.Fatal(fs, err)
	}
	f := fs[0]
	if f.GitProvider != "gitweb" || f.GitMode != "tags" || f.GitTagFilter != "v3.4.*" || f.Icon != srv.URL+"/static/git.png" {
		t.Fatal(f)
	}
	a.poller.fetch(t.Context(), f.ID)
	fs, err = a.store.Feeds()
	if err != nil || fs[0].Count != 2 || fs[0].Error != "" || calls.Load() != 3 {
		t.Fatal(fs, err, calls.Load())
	}
	a.poller.fetch(t.Context(), f.ID)
	if calls.Load() != 3 {
		t.Fatal("tag details were not cached", calls.Load())
	}
	token := approvedToken(t, a)
	w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", nil)
	var stream struct {
		Items []struct {
			Title     string
			Published int64
			Summary   struct{ Content string }
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &stream); err != nil || len(stream.Items) != 2 {
		t.Fatal(w.Body.String(), err)
	}
	for _, item := range stream.Items {
		wantDate := time.Date(2026, 10, 14, 13, 57, 49, 0, time.UTC).Unix()
		if item.Title == "v3.4.6" {
			wantDate = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC).Unix()
		}
		if item.Published != wantDate || !strings.Contains(item.Summary.Content, "<li>Commit fix</li>") || strings.Contains(item.Summary.Content, "PGP") {
			t.Fatal(item)
		}
		if item.Title == "v3.4.7" && !strings.Contains(item.Summary.Content, "<strong>Fixed</strong>") {
			t.Fatal(item)
		}
	}
	form.Set("id", fmt.Sprint(f.ID))
	form.Set("tag_filter", "v3.4.7")
	w = asyncAdmin(a, cookie, "POST", "/git-source", form)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", nil)
	if err = json.Unmarshal(w.Body.Bytes(), &stream); err != nil || len(stream.Items) != 1 || stream.Items[0].Title != "v3.4.7" {
		t.Fatal(w.Body.String(), err)
	}
	w = apiReq(a, token, "GET", "/reader/api/0/unread-count", nil)
	if !strings.Contains(w.Body.String(), `"count":1`) || strings.Contains(w.Body.String(), `"count":2`) {
		t.Fatal(w.Body.String())
	}
	var exported strings.Builder
	if err = a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err = other.store.ImportROI(strings.NewReader(exported.String())); err != nil {
		t.Fatal(err, exported.String())
	}
	copied, err := other.store.Feeds()
	if err != nil || len(copied) != 1 || copied[0].GitTagFilter != "v3.4.7" || copied[0].GitProvider != "gitweb" {
		t.Fatal(copied, err)
	}
	// Clearing the filter and enabling previews downloads all four distinct tags.
	form.Set("tag_filter", "")
	form.Set("include_preview", "1")
	if w = asyncAdmin(a, cookie, "POST", "/git-source", form); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a.poller.fetch(t.Context(), f.ID)
	fs, _ = a.store.Feeds()
	if fs[0].Count != 4 {
		t.Fatal(fs)
	}
	form.Set("tag_filter", "[invalid]")
	if w = asyncAdmin(a, cookie, "POST", "/git-source", form); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestGitwebErrorsLimitsAndDetails(t *testing.T) {
	a := testApp(t)
	source, _ := parseGitSource("https://example.org/?p=a.git", "gitweb", "tags", false)
	if _, err := parseGitwebTags([]byte(`<html>Error</html>`), source); err == nil {
		t.Fatal("accepted HTML error")
	}
	tags, err := parseGitwebTags([]byte(`<table class="tags"><tr><td><a href="?p=other.git;a=shortlog;h=refs/tags/v1">other</a><a href="%zz">bad</a></td></tr></table>`), source)
	if err != nil || len(tags) != 0 {
		t.Fatal(tags, err)
	}
	if _, _, _, err := parseGitwebDetail([]byte(`<div class="page_body">No date</div>`)); err == nil {
		t.Fatal("invented date")
	}
	for _, date := range []string{"2026-10-14T13:57:49Z", "Wed, 14 Oct 2026 13:57:49 GMT", "Fri, 3 Jul 2026 08:20:43 +0000"} {
		if _, _, d, err := parseGitwebDetail([]byte(`<span class="datetime">` + date + `</span>`)); err != nil || d == nil {
			t.Fatal(d, err)
		}
	}
	for _, mode := range []string{"http", "invalid", "detail", "date", "budget", "limit", "closed"} {
		t.Run(mode, func(t *testing.T) {
			count := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q, _ := gitwebQuery(r.URL.RawQuery)
				if mode == "http" {
					http.Error(w, "down", http.StatusServiceUnavailable)
					return
				}
				if mode == "invalid" {
					fmt.Fprint(w, "not gitweb")
					return
				}
				if q.Get("a") == "tags" {
					fmt.Fprint(w, `<table class="tags">`)
					n := 1
					if mode == "limit" {
						n = 25
					}
					for i := 0; i < n; i++ {
						fmt.Fprintf(w, `<tr><td><a href="?p=a.git;a=shortlog;h=refs/tags/v%d">tag</a></td></tr>`, i)
					}
					fmt.Fprint(w, `</table>`)
					return
				}
				count++
				if mode == "detail" {
					http.Error(w, "down", http.StatusServiceUnavailable)
					return
				}
				if mode == "date" {
					fmt.Fprint(w, `<div class="page_body">No date</div>`)
					return
				}
				fmt.Fprint(w, `<span class="datetime">2026-10-14T13:57:49Z</span><div class="page_body">Notes</div>`)
			}))
			defer srv.Close()
			app := a
			if mode == "closed" {
				app = testApp(t)
			}
			s, _ := parseGitSource(srv.URL+"/?p=a.git", "gitweb", "tags", false)
			if mode == "budget" {
				old := app.poller.config.Poll.MaxBytes
				app.poller.config.Poll.MaxBytes = 50
				defer func() { app.poller.config.Poll.MaxBytes = old }()
			}
			if mode == "closed" {
				if err := app.store.DB.Close(); err != nil {
					t.Fatal(err)
				}
			}
			feed, err := app.poller.fetchGit(t.Context(), s, 0)
			if mode == "limit" {
				if err != nil || len(feed.Items) != 20 || count != 20 {
					t.Fatal(feed, err, count)
				}
			} else if err == nil {
				t.Fatal("expected failure", mode)
			}
		})
	}
	// Reject HTML error pages during subscription discovery too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not a repository") }))
	defer srv.Close()
	source, _ = parseGitSource(srv.URL+"/?p=a.git", "auto", "tags", false)
	if _, err := a.poller.detectGit(t.Context(), source); err == nil {
		t.Fatal("discovered nonexistent repository")
	}
}

func TestTagFilterAcrossProvidersAndValidation(t *testing.T) {
	for _, mode := range []string{"releases", "tags"} {
		t.Run(mode, func(t *testing.T) {
			a, srv := fixtureGit(t, "forgejo")
			s, err := parseGitSource(srv.URL+"/team/project", "forgejo", mode, true)
			if err != nil {
				t.Fatal(err)
			}
			s.TagFilter = "v2.*"
			id := storeGit(t, a, s)
			a.poller.fetch(t.Context(), id)
			var count int
			if err = a.store.DB.QueryRow("SELECT count(*) FROM entries WHERE feed_id=?", id).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			s.TagFilter = "[bad]"
			if err = a.store.SaveGitSource(id, s); err == nil {
				t.Fatal("saved invalid filter")
			}
		})
	}
	for _, entry := range []roiSource{{Type: "rss", URL: "https://example.org/rss", TagFilter: "v*"}, {Type: "git", Repository: "https://example.org/?p=a.git", Service: "gitweb", TagFilter: "[bad]"}} {
		if _, err := normalizeSubscription(entry); err == nil {
			t.Fatal("imported invalid filter", entry)
		}
	}
	sub, err := normalizeSubscription(roiSource{Type: "git", Repository: "https://example.org/?p=a.git", Service: "gitweb", TagFilter: "v*"})
	if err != nil || sub.Source.Mode != "tags" || sub.Source.TagFilter != "v*" {
		t.Fatal(sub, err)
	}
	a := testApp(t)
	mustExec(t, a, "INSERT INTO feeds(url,title) VALUES('https://example.org/rss','Keep me'); ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=9")
	if err = migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	if err = migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	fs, err := a.store.Feeds()
	if err != nil || len(fs) != 1 || fs[0].Title != "Keep me" || fs[0].GitTagFilter != "" {
		t.Fatal(fs, err)
	}
}

// Opt-in network check; uses an isolated temporary database, never the user's feeds.
func TestGitwebLive(t *testing.T) {
	raw := os.Getenv("ROI_TEST_GITWEB_URL")
	if raw == "" {
		t.Skip("set ROI_TEST_GITWEB_URL to check a live Gitweb repository")
	}
	a := testApp(t)
	a.poller.config.Poll.Timeout = "30s"
	a.poller.config.Poll.MaxBytes = 32 << 20
	s, err := parseGitSource(raw, "auto", "tags", false)
	if err != nil {
		t.Fatal(err)
	}
	s.TagFilter = "v3.4.*"
	s, err = a.poller.detectGit(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	id := storeGit(t, a, s)
	feed, err := a.poller.fetchGit(t.Context(), s, id)
	if err != nil || len(feed.Items) == 0 {
		t.Fatal(feed, err)
	}
	for _, item := range feed.Items {
		if !matchesTag("v3.4.*", item.Title) || item.PublishedParsed == nil || item.Content == "" {
			t.Fatal(item)
		}
	}
	if !strings.Contains(feed.Items[0].Content, "following main changes") || !strings.Contains(feed.Items[0].Content, "<li>") {
		t.Fatal("release commit notes missing", feed.Items[0].Content)
	}
	t.Logf("%s: %d tags, newest %s (%s), icon %s", s.title(), len(feed.Items), feed.Items[0].Title, feed.Items[0].PublishedParsed, s.Icon)
}

func TestGitwebEnrichesExistingEntriesInPlace(t *testing.T) {
	a, srv, calls := fixtureGitweb(t)
	s, err := parseGitSource(srv.URL+"/?p=haproxy-3.4.git", "gitweb", "tags", false)
	if err != nil {
		t.Fatal(err)
	}
	s.TagFilter = "v3.4.7"
	id := storeGit(t, a, s)
	a.poller.fetch(t.Context(), id)
	var entryID, published int64
	if err = a.store.DB.QueryRow("SELECT id,published FROM entries WHERE feed_id=?", id).Scan(&entryID, &published); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE entries SET content='Old tag-only notes',url=?,is_read=1,starred=1 WHERE id=?", s.gitwebURL("tag", "v3.4.7"), entryID)
	a.poller.fetch(t.Context(), id)
	var gotID, gotDate int64
	var content, link string
	var read, starred bool
	if err = a.store.DB.QueryRow("SELECT id,published,content,url,is_read,starred FROM entries WHERE feed_id=?", id).Scan(&gotID, &gotDate, &content, &link, &read, &starred); err != nil {
		t.Fatal(err)
	}
	if gotID != entryID || gotDate != published || !read || !starred || !strings.Contains(content, "<li>Commit fix</li>") || link != s.gitwebURL("commit", "commit-v3.4.7") {
		t.Fatal(gotID, gotDate, read, starred, link, content)
	}
	before := calls.Load()
	a.poller.fetch(t.Context(), id)
	if calls.Load() != before {
		t.Fatal("enriched notes were not cached")
	}
}

func TestGitwebCommitNotesVariantsAndFailures(t *testing.T) {
	const list = `<table class="tags"><tr><td><a href="?p=a.git;a=tag;h=taghash">tag</a><a href="?p=a.git;a=shortlog;h=refs/tags/v1">log</a></td></tr></table>`
	const date = `<span class="datetime">2026-10-14T13:57:49Z</span>`
	for _, mode := range []string{"http", "date", "budget", "same", "empty-tag", "empty-commit", "author"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			tagBody := "Tag notes"
			commitBody := "Commit notes"
			if mode == "same" {
				commitBody = tagBody
			}
			if mode == "empty-tag" {
				tagBody = ""
			}
			if mode == "empty-commit" {
				commitBody = ""
			}
			tagPage := date + `<div class="page_body">` + tagBody + `</div>`
			commitPage := date + `<table><tr><td>author</td><td>Commit author</td></tr></table><div class="page_body">` + commitBody + `</div>`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q, _ := gitwebQuery(r.URL.RawQuery)
				switch q.Get("a") {
				case "tags":
					fmt.Fprint(w, list)
				case "tag":
					fmt.Fprint(w, tagPage)
				case "commit":
					if mode == "http" {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					if mode == "date" {
						fmt.Fprint(w, `<div class="page_body">No date</div>`)
						return
					}
					fmt.Fprint(w, commitPage)
				}
			}))
			defer srv.Close()
			if mode == "budget" {
				a.poller.config.Poll.MaxBytes = int64(len(list) + len(tagPage) + len(commitPage) - 1)
			}
			s, err := parseGitSource(srv.URL+"/?p=a.git", "gitweb", "tags", false)
			if err != nil {
				t.Fatal(err)
			}
			feed, err := a.poller.fetchGit(t.Context(), s, 0)
			if mode == "http" || mode == "date" || mode == "budget" {
				if err == nil {
					t.Fatal("missing commit failure", mode)
				}
				return
			}
			if err != nil || len(feed.Items) != 1 {
				t.Fatal(feed, err)
			}
			content := feed.Items[0].Content
			if commitBody != "" && !strings.Contains(content, commitBody) {
				t.Fatal(content)
			}
			if tagBody != "" && !strings.Contains(content, tagBody) {
				t.Fatal(content)
			}
			if mode == "same" && strings.Count(content, "Tag notes") != 1 {
				t.Fatal("duplicate notes", content)
			}
			if mode == "author" && feed.Items[0].Author.Name != "Commit author" {
				t.Fatal(feed.Items[0].Author)
			}
		})
	}
}

func TestGitwebUsesLinkedCommitObject(t *testing.T) {
	s, _ := parseGitSource("https://example.org/?p=a.git", "gitweb", "tags", false)
	tags, err := parseGitwebTags([]byte(`<table class="tags"><tr><td><a href="?p=a.git;a=commit;h=commit-object">v1</a><a href="?p=a.git;a=tag;h=tag-object">tag</a><a href="?p=a.git;a=shortlog;h=refs/tags/v1">log</a></td></tr></table>`), s)
	if err != nil || len(tags) != 1 || tags[0].Commit != "commit-object" || tags[0].Ref != "tag-object" {
		t.Fatal(tags, err)
	}
}

func TestGitwebMessageLayout(t *testing.T) {
	body, _, _, err := parseGitwebDetail([]byte(`<span class="datetime">2026-10-14T13:57:49Z</span><div class="page_body">Notes<br/>
<br/>
Changes:<br/>
&nbsp;&nbsp;&nbsp;&nbsp;- Fixed<br/>
&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;continued<br/>
&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;- Nested<br/>
<br/>
Other paragraph<br/>
    code()</div>`))
	if err != nil {
		t.Fatal(err)
	}
	want := "Notes\n\nChanges:\n- Fixed\n  continued\n    - Nested\n\nOther paragraph\n    code()"
	if body != want {
		t.Fatalf("got %q, want %q", body, want)
	}
	content, err := gitMarkdown(body, "https://example.org/")
	if err != nil || !strings.Contains(content, "<li>") || strings.Contains(content, "<pre><code>- Fixed") {
		t.Fatal(content, err)
	}
}
