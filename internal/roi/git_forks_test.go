package roi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGitReposForkVisibilityAndState(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "gitea", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			a, server, records := reposFixture(t, provider, "user")
			parent := &gitRepoParent{FullName: "upstream/original", Path: "upstream/original", HTMLURL: "https://example.org/upstream/original", WebURL: "https://example.org/upstream/original"}
			fork := gitRepoRecord{ID: 102, Name: "fork", Description: "Fork description", Created: "2024-01-01T00:00:00Z", HTMLURL: server.URL + "/team/fork", Owner: gitAccount{ID: 42}, Fork: true, Parent: parent}
			if provider == "gitlab" {
				fork.Fork = false
				fork.Parent = nil
				fork.ForkedFrom = parent
			}
			*records = append(*records, fork)
			source, err := parseGitReposSource(server.URL+"/team", provider, "user", false)
			if err != nil || !source.IgnoreForks {
				t.Fatal(source, err)
			}
			if err = a.store.SaveGitReposSource(0, source, nil); err != nil {
				t.Fatal(err)
			}
			feeds, _ := a.store.Feeds()
			id := feeds[0].ID
			a.poller.fetch(t.Context(), id)
			feeds, _ = a.store.Feeds()
			if feeds[0].Count != 2 || feeds[0].VisibleUnread != 1 || feeds[0].Error != "" {
				t.Fatal(feeds)
			}
			assertFeedAggregates(t, a)
			var entryID int64
			var body string
			var isFork bool
			if err = a.store.DB.QueryRow("SELECT id,content,git_fork FROM entries WHERE feed_id=? AND guid='gitrepo:102'", id).Scan(&entryID, &body, &isFork); err != nil || !isFork || !strings.Contains(body, "Fork of") || !strings.Contains(body, "upstream/original") {
				t.Fatal(body, isFork, err)
			}
			token := approvedToken(t, a)
			articleCount := func() int {
				t.Helper()
				w := apiReq(a, token, "GET", "/reader/api/0/stream/contents/feed/"+fmt.Sprint(id), nil)
				var response struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 {
					t.Fatal(w.Code, w.Body.String(), err)
				}
				return len(response.Items)
			}
			if articleCount() != 1 {
				t.Fatal("fork returned by default")
			}
			// Existing unread and starred state survives both visibility changes and sync.
			mustExec(t, a, "UPDATE entries SET starred=1 WHERE id=?", entryID)
			source.IgnoreForks = false
			if err = a.store.SaveGitReposSource(id, source, nil); err != nil {
				t.Fatal(err)
			}
			if articleCount() != 2 {
				t.Fatal("fork not restored immediately")
			}
			feeds, _ = a.store.Feeds()
			if feeds[0].VisibleUnread != 2 {
				t.Fatal(feeds)
			}
			assertFeedAggregates(t, a)
			var export strings.Builder
			if err = a.store.ExportROI(&export); err != nil || !strings.Contains(export.String(), "ignore_forks: false") {
				t.Fatal(export.String(), err)
			}
			clone := testApp(t)
			if err = clone.store.ImportROI(strings.NewReader(export.String())); err != nil {
				t.Fatal(err)
			}
			copied, _ := clone.store.Feeds()
			if copied[0].IgnoreForks {
				t.Fatal("false lost in round trip")
			}
			source.IgnoreForks = true
			if err = a.store.SaveGitReposSource(id, source, nil); err != nil {
				t.Fatal(err)
			}
			a.poller.fetch(t.Context(), id)
			var starred bool
			var sameID int64
			if err = a.store.DB.QueryRow("SELECT id,starred FROM entries WHERE guid='gitrepo:102' AND feed_id=?", id).Scan(&sameID, &starred); err != nil || sameID != entryID || !starred || articleCount() != 1 {
				t.Fatal(sameID, starred, err)
			}
			for _, query := range []string{"UPDATE entries SET is_read=1 WHERE git_fork=1", "UPDATE entries SET is_read=0 WHERE git_fork=1", "UPDATE entries SET git_fork=0 WHERE guid='gitrepo:102'", "UPDATE entries SET git_fork=1 WHERE guid='gitrepo:102'", "DELETE FROM entries WHERE git_fork=1"} {
				mustExec(t, a, query)
				assertFeedAggregates(t, a)
			}
		})
	}
}

func TestGitRepoForkFooter(t *testing.T) {
	for _, tc := range []struct {
		repo gitRepoRecord
		fork bool
		want string
	}{
		{gitRepoRecord{}, false, ""},
		{gitRepoRecord{Fork: true}, true, "Forked repository"},
		{gitRepoRecord{Fork: true, Parent: &gitRepoParent{HTMLURL: "javascript:bad", FullName: "bad"}}, true, "Forked repository"},
		{gitRepoRecord{Parent: &gitRepoParent{FullName: `a/<script>"bad"`, HTMLURL: "https://example.org/a?x=1&y=2"}}, true, "a/&lt;script&gt;&#34;bad&#34;"},
		{gitRepoRecord{ForkedFrom: &gitRepoParent{Path: "group/project", WebURL: "https://example.org/group/project"}}, true, "group/project"},
		{gitRepoRecord{Parent: &gitRepoParent{HTMLURL: "https://example.org/upstream"}}, true, "https://example.org/upstream"},
	} {
		fork, html := tc.repo.forkFooter()
		if fork != tc.fork || !strings.Contains(html, tc.want) || strings.Contains(html, "<script>") || strings.Contains(html, "javascript:") {
			t.Fatal(fork, html, tc)
		}
	}
}

func TestGitForkMetadataMakesNoDetailRequests(t *testing.T) {
	a := testApp(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v3/users/team":
			fmt.Fprint(w, `{"id":42,"login":"team","type":"User"}`)
		case "/api/v3/users/team/repos":
			fmt.Fprint(w, `[{"id":1,"fork":true,"name":"fork","html_url":"https://example.org/team/fork","created_at":"2024-01-01T00:00:00Z"}]`)
		default:
			t.Error("unexpected detail request", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	source, _ := parseGitReposSource(server.URL+"/team", "github", "user", false)
	feed, err := a.poller.fetchGitRepos(t.Context(), source, 1)
	if err != nil || calls != 2 || len(feed.Items) != 1 || !strings.Contains(feed.Items[0].Content, "Forked repository") || feed.Items[0].Custom["roi_fork"] != "1" {
		t.Fatal(calls, feed, err)
	}
}

func TestGitForkDefaultsMigrationAndAdmin(t *testing.T) {
	a, server, _ := reposFixture(t, "forgejo", "user")
	raw := "format: roi\nversion: 1\nsources:\n  - type: git_repos\n    url: " + server.URL + "/team\n    service: forgejo\n    paused: false\n"
	if err := a.store.ImportROI(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	feeds, _ := a.store.Feeds()
	id := feeds[0].ID
	if !feeds[0].IgnoreForks {
		t.Fatal("old ROI must default to ignore forks")
	}
	mustExec(t, a, "UPDATE feeds SET next_fetch=9999999999")
	rollbackForkMigration(t, a.store.DB)
	if err := migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	feeds, _ = a.store.Feeds()
	if !feeds[0].IgnoreForks || feeds[0].Next != 0 {
		t.Fatal(feeds)
	}
	cookie, csrf := adminSession(t, a)
	form := url.Values{"id": {fmt.Sprint(id)}, "csrf": {csrf}}
	for _, value := range []string{"", "1", ""} {
		form.Set("ignore_forks", value)
		w := asyncAdmin(a, cookie, "POST", "/git-repos", form)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		feeds, _ = a.store.Feeds()
		if feeds[0].IgnoreForks != (value == "1") {
			t.Fatal(feeds)
		}
	}
	yes := true
	for _, kind := range []string{"rss", "git"} {
		if _, err := normalizeSubscription(roiSource{Type: kind, IgnoreForks: &yes}); err == nil {
			t.Fatal("wrong source accepts ignore_forks", kind)
		}
	}
}
