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

func reposFixture(t *testing.T, provider, kind string) (*App, *httptest.Server, *[]gitRepoRecord) {
	t.Helper()
	a := testApp(t)
	var server *httptest.Server
	records := []gitRepoRecord{{ID: 101, Name: "project", Description: "Hello <script>bad()</script>\nWorld", Created: "2024-02-03T04:05:06Z", Owner: gitAccount{ID: 42}}}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := gitAccount{ID: 42, Login: "team", Username: "team", FullPath: "team", Type: "User", HTMLURL: server.URL + "/team", WebURL: server.URL + "/team", Avatar: server.URL + "/avatar.png"}
		if kind == "organization" {
			account.Type = "Organization"
		}
		switch {
		case r.URL.Path == "/api/forgejo/v1/version" && provider == "forgejo", r.URL.Path == "/api/v1/version" && provider == "gitea", r.URL.Path == "/api/v4/version" && provider == "gitlab":
			fmt.Fprint(w, `{"version":"1.0"}`)
		case strings.HasSuffix(r.URL.Path, "/users/team") && provider != "gitlab", strings.HasSuffix(r.URL.Path, "/orgs/team") && kind == "organization", strings.HasSuffix(r.URL.Path, "/groups/team") && kind == "group", strings.HasSuffix(r.URL.Path, "/groups/42") && kind == "group", strings.HasSuffix(r.URL.Path, "/users/42"):
			_ = json.NewEncoder(w).Encode(account)
		case r.URL.Path == "/api/v4/users" && kind == "user":
			_ = json.NewEncoder(w).Encode([]gitAccount{account})
		case strings.HasSuffix(r.URL.Path, "/repos"), strings.HasSuffix(r.URL.Path, "/projects"), strings.HasSuffix(r.URL.Path, "/repos/search"):
			q := r.URL.Query()
			if q.Get("page") != "1" {
				t.Errorf("unexpected page: %s", r.URL)
			}
			if provider == "gitlab" && (q.Get("order_by") != "created_at" || q.Get("visibility") != "public") {
				t.Errorf("query: %s", r.URL)
			}
			if kind == "group" && (q.Get("with_shared") != "false" || q.Get("include_subgroups") != "true") {
				t.Errorf("group query: %s", r.URL)
			}
			if provider == "forgejo" || provider == "gitea" {
				if q.Get("uid") != "42" || q.Get("exclusive") != "true" || q.Get("sort") != "created" {
					t.Errorf("query: %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": records})
			} else {
				_ = json.NewEncoder(w).Encode(records)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	records[0].HTMLURL = server.URL + "/team/project"
	return a, server, &records
}

func TestGitReposProvidersStateAndRoundTrip(t *testing.T) {
	for _, tc := range []struct{ service, kind string }{{"github", "organization"}, {"forgejo", "organization"}, {"gitea", "user"}, {"gitlab", "group"}, {"gitlab", "user"}} {
		t.Run(tc.service+"/"+tc.kind, func(t *testing.T) {
			a, server, records := reposFixture(t, tc.service, tc.kind)
			source, err := parseGitReposSource(server.URL+"/team", tc.service, "auto", tc.kind == "group")
			if err != nil {
				t.Fatal(err)
			}
			source, err = a.poller.discoverGitRepos(t.Context(), source)
			if err != nil || source.OwnerID != 42 || source.OwnerType != tc.kind {
				t.Fatal(source, err)
			}
			if err = a.store.SaveGitReposSource(0, source, nil); err != nil {
				t.Fatal(err)
			}
			feeds, _ := a.store.Feeds()
			id := feeds[0].ID
			a.poller.fetch(t.Context(), id)
			feeds, err = a.store.Feeds()
			if err != nil || feeds[0].Count != 1 || feeds[0].Error != "" || feeds[0].Kind != kindGitRepos || feeds[0].Folders[0] != gitReposFolder || feeds[0].Icon != server.URL+"/avatar.png" || feeds[0].Site != server.URL+"/team" {
				t.Fatal(feeds, err)
			}
			var entryID, published int64
			var body string
			if err = a.store.DB.QueryRow("SELECT id,published,content FROM entries WHERE feed_id=?", id).Scan(&entryID, &published, &body); err != nil {
				t.Fatal(err)
			}
			if published != 1706933106 || strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
				t.Fatal(published, body)
			}
			mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE id=?", entryID)
			(*records)[0].Name = "renamed"
			(*records)[0].Description = "updated"
			a.poller.fetch(t.Context(), id)
			var title string
			var read, starred bool
			var newID int64
			if err = a.store.DB.QueryRow("SELECT id,title,is_read,starred FROM entries WHERE feed_id=?", id).Scan(&newID, &title, &read, &starred); err != nil || newID != entryID || title != "renamed" || !read || !starred {
				t.Fatal(newID, title, read, starred, err)
			}
			var out strings.Builder
			if err = a.store.ExportROI(&out); err != nil {
				t.Fatal(err)
			}
			other := testApp(t)
			if err = other.store.ImportROI(strings.NewReader(out.String())); err != nil {
				t.Fatal(out.String(), err)
			}
			copied, _ := other.store.Feeds()
			if len(copied) != 1 || copied[0].Kind != kindGitRepos || copied[0].IncludeSubgroups != (tc.kind == "group") {
				t.Fatal(copied)
			}
			token := approvedToken(t, a)
			w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), gitReposFolder) {
				t.Fatal(w.Body.String())
			}
			if w = apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"s": {"feed/" + fmt.Sprint(id)}, "ac": {"edit"}, "a": {"user/1/label/" + gitReposFolder}}); w.Code != 403 {
				t.Fatal(w.Code, w.Body.String())
			}
			empty := []gitRepoRecord{}
			*records = empty
			a.poller.fetch(t.Context(), id)
			feeds, _ = a.store.Feeds()
			if feeds[0].Count != 1 {
				t.Fatal("history lost", feeds)
			}
			if err = a.store.ResetFeed(id); err != nil {
				t.Fatal(err)
			}
			feeds, _ = a.store.Feeds()
			if feeds[0].Count != 0 {
				t.Fatal(feeds)
			}
		})
	}
}

func TestGitReposAdminAndValidation(t *testing.T) {
	a, server, _ := reposFixture(t, "forgejo", "organization")
	cookie, csrf := adminSession(t, a)
	form := url.Values{"id": {"0"}, "account": {server.URL + "/team"}, "provider": {"auto"}, "owner_type": {"auto"}, "csrf": {csrf}}
	if w := asyncAdmin(a, nil, "POST", "/git-repos", form); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, cookie, "POST", "/git-repos", form); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, _ := a.store.Feeds()
	id := feeds[0].ID
	form.Set("id", fmt.Sprint(id))
	form.Set("icon_override", server.URL+"/custom.png")
	if w := asyncAdmin(a, cookie, "POST", "/git-repos", form); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, _ = a.store.Feeds()
	if feeds[0].Icon != server.URL+"/custom.png" {
		t.Fatal(feeds)
	}
	page := adminRequest(a, cookie, "GET", "/feeds", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "data-repos-edit") || !strings.Contains(page.Body.String(), "GIT Repos Feeds") {
		t.Fatal(page.Code)
	}
	for _, bad := range []string{"bad", "-1", "9999"} {
		form.Set("id", bad)
		if w := asyncAdmin(a, cookie, "POST", "/git-repos", form); w.Code < 400 {
			t.Fatal(bad, w.Code)
		}
	}
	for _, tc := range []struct {
		raw, service, kind string
		sub                bool
	}{
		{"https://example.org/", "github", "auto", false}, {"https://example.org/a/b", "github", "auto", false},
		{"https://example.org/a?x=y", "gitlab", "auto", false}, {"https://example.org/a%2fb", "gitlab", "auto", false},
		{"https://example.org/a", "gitweb", "auto", false}, {"https://example.org/a", "github", "group", false},
		{"https://example.org/a", "gitlab", "organization", false}, {"https://example.org/a", "gitlab", "user", true},
		{"https://example.org/a", "gitlab", "other", false}, {"https://example.org/a", "gitea", "user", true},
	} {
		if _, err := parseGitReposSource(tc.raw, tc.service, tc.kind, tc.sub); err == nil {
			t.Fatal(tc)
		}
	}
	if _, err := normalizeSubscription(roiSource{Type: kindGitRepos, URL: server.URL + "/team", Service: "auto"}); err == nil {
		t.Fatal("auto import")
	}
	if _, err := normalizeSubscription(roiSource{Type: kindGitRepos, URL: server.URL + "/team", Service: "forgejo", Mode: "tags"}); err == nil {
		t.Fatal("mixed import")
	}
	if err := a.store.SaveGitReposSource(0, GitSource{URL: server.URL + "/team", Provider: "forgejo"}, nil); err == nil {
		t.Fatal("duplicate")
	}
}

func TestGitReposPaginationFiltersAndErrors(t *testing.T) {
	a := testApp(t)
	mode := "pages"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/users/") {
			fmt.Fprint(w, `{"id":42,"login":"team"}`)
			return
		}
		calls++
		if mode == "error" {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		if mode == "bad" {
			fmt.Fprint(w, `{"data":broken}`)
			return
		}
		w.Header().Set("Link", `<https://example.org/next>; rel="next"`)
		rows := []gitRepoRecord{}
		for i := 0; i < 30; i++ {
			rows = append(rows, gitRepoRecord{ID: int64((calls-1)*30 + i + 1), Name: "repo", HTMLURL: "https://example.org/r", Created: "2024-01-01T00:00:00Z"})
		}
		if mode == "filtered" {
			rows = []gitRepoRecord{{ID: 1, Private: true}, {ID: 2, Visibility: "private"}, {ID: 3, Owner: gitAccount{ID: 99}}, {ID: 4, Name: "no date"}, {ID: 5, Name: "bad link", Created: "2024-01-01T00:00:00Z", HTMLURL: "javascript:bad"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	defer server.Close()
	source, _ := parseGitReposSource(server.URL+"/team", "gitea", "user", false)
	feed, err := a.poller.fetchGitRepos(t.Context(), source, 1)
	if err != nil || len(feed.Items) != 100 || calls != 4 {
		t.Fatal(calls, err)
	}
	mode = "filtered"
	calls = 0
	feed, err = a.poller.fetchGitRepos(t.Context(), source, 1)
	if err != nil || len(feed.Items) != 0 || calls != 5 || feed.Custom["roi_repos_partial"] != "1" {
		t.Fatal(calls, feed, err)
	}
	for _, state := range []string{"error", "bad"} {
		mode = state
		if _, err = a.poller.fetchGitRepos(t.Context(), source, 1); err == nil {
			t.Fatal(state)
		}
	}
	source.OwnerID = 43
	if _, err = a.poller.fetchGitRepos(t.Context(), source, 1); err == nil {
		t.Fatal("identity mismatch")
	}
}

func TestGitReposMigrationAndReservedFolders(t *testing.T) {
	a := testApp(t)
	rss := addFeed(t, a, "https://example.org/feed")
	source, _ := parseGitSource("https://github.com/team/project", "github", "releases", false)
	git := storeGit(t, a, source)
	rollbackForkMigration(t, a.store.DB)
	_, err := a.store.DB.Exec(`DROP TRIGGER git_repos_folder_only; DROP TRIGGER git_releases_kind_only;
 ALTER TABLE feeds DROP COLUMN source_kind;
 ALTER TABLE feeds DROP COLUMN git_owner_type;
 ALTER TABLE feeds DROP COLUMN git_owner_id;
 ALTER TABLE feeds DROP COLUMN git_include_subgroups;
 ALTER TABLE feeds DROP COLUMN repos_partial;
 PRAGMA user_version=19;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("INSERT INTO folders(name) VALUES('GIT_Repos'),('GIT_Repos/Old'),('RSS/GIT_Repos'); INSERT INTO feed_folders(feed_id,name) VALUES(?, 'GIT_Repos'),(?, 'GIT_Repos/Old'),(?, 'RSS/GIT_Repos')", rss, rss, rss); err != nil {
		t.Fatal(err)
	}
	if err = migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	var preserved int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM feed_folders WHERE feed_id=? AND name IN ('RSS/GIT_Repos','RSS/GIT_Repos/Old')", rss).Scan(&preserved); err != nil || preserved != 2 {
		t.Fatal("RSS folder migration lost membership", preserved, err)
	}
	feeds, _ := a.store.Feeds()
	kinds := map[int64]string{}
	for _, f := range feeds {
		kinds[f.ID] = f.Kind
	}
	if kinds[rss] != kindRSS || kinds[git] != kindGitReleases {
		t.Fatal(kinds)
	}
	mustExec(t, a, "INSERT INTO folders(name) VALUES(?)", gitReposFolder)
	if _, err = a.store.DB.Exec("INSERT INTO feed_folders(feed_id,name) VALUES(?,?)", rss, gitReposFolder); err == nil {
		t.Fatal("RSS entered reserved folder")
	}
	if err = migrate(a.store.DB); err != nil {
		t.Fatal("migration not idempotent", err)
	}
	token := approvedToken(t, a)
	for _, tc := range []struct {
		route string
		form  url.Values
	}{
		{"subscription/quickadd", url.Values{"quickadd": {"https://example.org/new"}, "a": {"user/1/label/GIT_Repos"}}},
		{"rename-tag", url.Values{"s": {"user/1/label/GIT_Repos"}, "dest": {"user/1/label/Other"}}},
		{"disable-tag", url.Values{"s": {"user/1/label/GIT_Repos"}}},
	} {
		if w := apiReq(a, token, "POST", "/reader/api/0/"+tc.route, tc.form); w.Code != 403 {
			t.Fatal(tc.route, w.Code, w.Body.String())
		}
	}
}

func TestGitReposProfileFailureVariants(t *testing.T) {
	for _, tc := range []struct {
		provider, kind, body string
		status               int
	}{
		{"github", "user", `{"id":1,"type":"Organization"}`, 200},
		{"github", "auto", `{"login":"missing-id"}`, 200},
		{"github", "auto", `{"id":`, 200},
		{"gitlab", "group", "missing", 404},
		{"gitlab", "user", `[]`, 200},
		{"forgejo", "organization", "busy", 500},
	} {
		t.Run(tc.provider+"/"+tc.kind+"/"+tc.body, func(t *testing.T) {
			a := testApp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			source, _ := parseGitReposSource(server.URL+"/team", tc.provider, tc.kind, false)
			if _, err := a.poller.fetchGitRepos(t.Context(), source, 1); err == nil {
				t.Fatal("profile failure ignored")
			}
		})
	}
	for _, provider := range []string{"gitea", "gitlab"} {
		a, server, _ := reposFixture(t, provider, "user")
		source, _ := parseGitReposSource(server.URL+"/team", "auto", "auto", false)
		detected, err := a.poller.discoverGitRepos(t.Context(), source)
		if err != nil || detected.Provider != provider {
			t.Fatal(detected, err)
		}
	}
}

func TestGitReposDiscoveryAndStoreFailures(t *testing.T) {
	a := testApp(t)
	for host, want := range map[string]string{"github.com": "github", "gitlab.com": "gitlab", "codeberg.org": "forgejo"} {
		s, _ := parseGitReposSource("https://"+host+"/team", "auto", "auto", false)
		got, err := a.poller.detectReposProvider(t.Context(), s, &gitBudget{Limit: 1024})
		if err != nil || got.Provider != want {
			t.Fatal(got, err)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := testApp(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, "{}") }))
			defer server.Close()
			s, _ := parseGitReposSource(server.URL+"/team", "auto", "auto", false)
			if _, err := a.poller.discoverGitRepos(t.Context(), s); err == nil {
				t.Fatal("unknown service accepted")
			}
		})
	}
	for _, g := range []GitSource{
		{URL: "bad", Provider: "github"},
		{URL: "https://example.org/team", Provider: "auto"},
		{URL: "https://example.org/team", Provider: "gitea", Icon: "javascript:bad"},
	} {
		if err := a.store.SaveGitReposSource(0, g, nil); err == nil {
			t.Fatal(g)
		}
	}
	bad := "javascript:bad"
	s, _ := parseGitReposSource("https://example.org/team", "gitea", "user", false)
	if err := a.store.SaveGitReposSource(0, s, &bad); err == nil {
		t.Fatal("unsafe icon accepted")
	}
	if err := a.store.SaveGitReposSource(999, s, nil); err == nil {
		t.Fatal("unknown source updated")
	}
	for _, table := range []string{"feeds", "folders", "feed_folders"} {
		t.Run(table, func(t *testing.T) {
			a := testApp(t)
			mustExec(t, a, "CREATE TRIGGER reject_repos BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'failure'); END")
			if err := a.store.SaveGitReposSource(0, s, nil); err == nil {
				t.Fatal("write failure ignored")
			}
			var count int
			if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM feeds").Scan(&count); err != nil || count != 0 {
				t.Fatal("partial save", count, err)
			}
		})
	}
}

func TestGitReposGitHubAuthorizationAndConditionalCache(t *testing.T) {
	a := testApp(t)
	a.poller.config.Git.Tokens = map[string]string{"github.com": "test-repos-token"}
	requests, hits := 0, 0
	a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer test-repos-token" {
			t.Fatal("incorrect token scope", r.URL)
		}
		if r.Header.Get("If-None-Match") == `"repos-v1"` {
			hits++
			return gitResponse(r, http.StatusNotModified, ""), nil
		}
		var body string
		switch r.URL.Path {
		case "/users/team":
			body = `{"id":42,"login":"team","type":"Organization","html_url":"https://github.com/team","avatar_url":"https://avatars.githubusercontent.com/u/42?v=4"}`
		case "/users/team/repos":
			if r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("direction") != "desc" || r.URL.Query().Get("per_page") != "100" {
				t.Fatal(r.URL)
			}
			body = `[{"id":123,"name":"project","html_url":"https://github.com/team/project","created_at":"2024-01-01T00:00:00Z","owner":{"id":42}}]`
		default:
			t.Fatal("unexpected API request", r.URL)
		}
		response := gitResponse(r, http.StatusOK, body)
		response.Header.Set("ETag", `"repos-v1"`)
		return response, nil
	})
	source, _ := parseGitReposSource("https://github.com/team", "github", "organization", false)
	for range 2 {
		feed, err := a.poller.fetchGitRepos(t.Context(), source, 7)
		if err != nil || len(feed.Items) != 1 || feed.Custom["roi_owner_avatar"] != "https://avatars.githubusercontent.com/u/42?v=4" {
			t.Fatal(feed, err)
		}
	}
	if requests != 4 || hits != 2 {
		t.Fatal(requests, hits)
	}
}

func TestGitReposRejectsWrongResponseShapes(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "forgejo", "gitea"} {
		for _, body := range []string{"null", `{}`, `{"data":null}`, `{"data":{}}`, `{"data":[]}`} {
			t.Run(provider+"/"+body, func(t *testing.T) {
				a := testApp(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/repos") || strings.HasSuffix(r.URL.Path, "/projects") {
						fmt.Fprint(w, body)
						return
					}
					if r.URL.Path == "/api/v4/users" {
						fmt.Fprint(w, `[{"id":42,"username":"team"}]`)
						return
					}
					fmt.Fprint(w, `{"id":42,"login":"team"}`)
				}))
				defer server.Close()
				source, _ := parseGitReposSource(server.URL+"/team", provider, "user", false)
				_, err := a.poller.fetchGitRepos(t.Context(), source, 1)
				valid := (provider == "forgejo" || provider == "gitea") && body == `{"data":[]}`
				if (err == nil) != valid {
					t.Fatal(body, err)
				}
			})
		}
	}
}

func TestGitReposGitLabGroupOmitsEmbeddedProjects(t *testing.T) {
	a := testApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects") {
			fmt.Fprint(w, "[]")
			return
		}
		if r.URL.Query().Get("with_projects") != "false" {
			t.Error("group profile downloads redundant projects")
		}
		fmt.Fprint(w, `{"id":42,"full_path":"group/subgroup","web_url":"https://example.org/groups/group/subgroup"}`)
	}))
	defer server.Close()
	source, _ := parseGitReposSource(server.URL+"/group/subgroup", "gitlab", "group", true)
	for _, id := range []int64{0, 42} {
		source.OwnerID = id
		feed, err := a.poller.fetchGitRepos(t.Context(), source, 1)
		if err != nil || feed.Link != "https://example.org/groups/group/subgroup" {
			t.Fatal(feed, err)
		}
	}
}
