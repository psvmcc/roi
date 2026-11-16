package roi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

const stableVersionRegex = `^v[0-9]+\.[0-9]+\.[0-9]+$`

func TestTagRegexStrictVersionsAndValidation(t *testing.T) {
	for _, tag := range []string{"v1.10.1", "v1.10.12", "v18.11.0", "v123.456.789"} {
		if !matchesTag(stableVersionRegex, tag, "regex") || !matchesTag(`v[0-9]+\.[0-9]+\.[0-9]+`, tag, "regex") {
			t.Fatal(tag)
		}
	}
	for _, tag := range []string{"v1.10.1-lalala", "v1.10.1-rc1", "v1.10.1+build", "api/v1.10.1", "v1.10", "v1.10.1.2", "prefixv1.10.1", "V1.10.1", "v1x10x1", "v1.10.1\n"} {
		if matchesTag(stableVersionRegex, tag, "regex") {
			t.Fatal(tag)
		}
	}
	for _, pattern := range []string{"[", `(a)\1`, "(?=v)v", "(?<=v)1", strings.Repeat("a", 257), "a\nb", string([]byte{0xff})} {
		if validateTagFilter(pattern, "regex") == nil {
			t.Fatal("invalid regex accepted", pattern)
		}
	}
	if validateTagFilter(stableVersionRegex) == nil || validateTagFilter("v*", "bad") == nil || matchesTag("v*", "v1", "bad") {
		t.Fatal("syntax validation")
	}
	if !matchesTag("v1.10.*", "v1.10.1-lalala") || !matchesTag("v?.?.?", "v1.2.3") || !matchesTag("", "any", "regex") {
		t.Fatal("legacy glob or empty pattern changed")
	}
}

func regexItems(t *testing.T, a *App, token string, id int64) []struct{ ID, Title string } {
	t.Helper()
	w := apiReq(a, token, "GET", "/reader/api/0/stream/contents/feed/"+fmt.Sprint(id), nil)
	var stream struct{ Items []struct{ ID, Title string } }
	if err := json.Unmarshal(w.Body.Bytes(), &stream); err != nil || w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	return stream.Items
}

func TestTagRegexStoredVisibilityCountersAndRoundTrip(t *testing.T) {
	a, server := fixtureGit(t, "gitea")
	source, _ := parseGitSource(server.URL+"/owner/project", "gitea", "tags", true)
	id := storeGit(t, a, source)
	tags := []string{"v1.10.1", "v1.10.12", "v18.11.0", "v1.10.1-lalala", "api/v1.10.1", "v1.10.1+build", "v1.10", "v1.10.1.2"}
	for _, tag := range tags {
		save(t, a, id, &gofeed.Item{GUID: "git:" + tag, Title: tag, Link: server.URL + "/tag/" + tag})
	}
	var originalID int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=? AND guid='git:v1.10.1'", id).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE id=?", originalID)
	source.TagFilter, source.TagFilterMode = stableVersionRegex, "regex"
	if err := a.store.SaveGitSource(id, source); err != nil {
		t.Fatal(err)
	}
	token := approvedToken(t, a)
	items := regexItems(t, a, token, id)
	if len(items) != 3 {
		t.Fatal(items)
	}
	feeds, _ := a.store.Feeds()
	if feeds[0].Count != 8 || feeds[0].VisibleUnread != 2 || feeds[0].GitTagFilterMode != "regex" {
		t.Fatal(feeds)
	}
	assertFeedAggregates(t, a)
	var hiddenID int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE guid='git:v1.10.1-lalala' AND feed_id=?", id).Scan(&hiddenID); err != nil {
		t.Fatal(err)
	}
	hidden := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {fmt.Sprint(hiddenID)}})
	if hidden.Code != http.StatusOK || strings.Contains(hidden.Body.String(), "v1.10.1-lalala") {
		t.Fatal("direct ID leaked hidden article", hidden.Code, hidden.Body.String())
	}
	for _, query := range []string{"UPDATE entries SET is_read=1 WHERE guid='git:v1.10.12'", "UPDATE entries SET is_read=0 WHERE guid='git:v1.10.12'", "UPDATE entries SET content='changed' WHERE guid='git:v1.10.1-lalala'"} {
		mustExec(t, a, query)
		assertFeedAggregates(t, a)
	}
	var out strings.Builder
	if err := a.store.ExportROI(&out); err != nil || !strings.Contains(out.String(), "tag_filter_mode: regex") {
		t.Fatal(out.String(), err)
	}
	clone := testApp(t)
	if err := clone.store.ImportROI(strings.NewReader(out.String())); err != nil {
		t.Fatal(err)
	}
	copied, _ := clone.store.Feeds()
	if copied[0].GitTagFilterMode != "regex" || copied[0].GitTagFilter != stableVersionRegex {
		t.Fatal(copied)
	}
	source.TagFilter = `v1\.10\.[0-9]+`
	if err := a.store.SaveGitSource(id, source); err != nil {
		t.Fatal(err)
	}
	if len(regexItems(t, a, token, id)) != 2 {
		t.Fatal("regex change did not update saved visibility")
	}
	source.TagFilterMode = "glob"
	source.TagFilter = "v1.10.*"
	if err := a.store.SaveGitSource(id, source); err != nil {
		t.Fatal(err)
	}
	if len(regexItems(t, a, token, id)) != 5 {
		t.Fatal("glob switch did not restore matching suffixes")
	}
	source.TagFilterMode = "regex"
	source.TagFilter = ""
	if err := a.store.SaveGitSource(id, source); err != nil {
		t.Fatal(err)
	}
	if len(regexItems(t, a, token, id)) != 8 {
		t.Fatal("empty regex did not restore all entries")
	}
	var sameID int64
	var read, starred bool
	if err := a.store.DB.QueryRow("SELECT id,is_read,starred FROM entries WHERE feed_id=? AND guid='git:v1.10.1'", id).Scan(&sameID, &read, &starred); err != nil || sameID != originalID || !read || !starred {
		t.Fatal(sameID, read, starred, err)
	}
	assertFeedAggregates(t, a)
}

func TestTagRegexProvidersAndGitweb(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "gitea", "forgejo"} {
		for _, mode := range []string{"tags", "releases"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				a, server := fixtureGit(t, provider)
				source, _ := parseGitSource(server.URL+"/owner/project", provider, mode, true)
				source.TagFilterMode = "regex"
				source.TagFilter = stableVersionRegex
				source, err := a.poller.detectGit(t.Context(), source)
				if err != nil || source.TagFilterMode != "regex" {
					t.Fatal(source, err)
				}
				id := storeGit(t, a, source)
				a.poller.fetch(t.Context(), id)
				feeds, _ := a.store.Feeds()
				want := int64(1)
				if mode == "releases" {
					want = 2
				}
				if feeds[0].Error != "" || feeds[0].Count != want {
					t.Fatal(feeds)
				}
				assertFeedAggregates(t, a)
			})
		}
	}
	a, server, _ := fixtureGitweb(t)
	source, _ := parseGitSource(server.URL+"/?p=haproxy-3.4.git;a=summary", "gitweb", "tags", true)
	source.TagFilterMode = "regex"
	source.TagFilter = `^v3\.4\.[0-9]+$`
	id := storeGit(t, a, source)
	a.poller.fetch(t.Context(), id)
	feeds, _ := a.store.Feeds()
	if feeds[0].Count != 2 || feeds[0].Error != "" {
		t.Fatal(feeds)
	}
	assertFeedAggregates(t, a)
}

func TestTagRegexAdminValidationMigrationAndAtomicity(t *testing.T) {
	a, server := fixtureGit(t, "gitea")
	cookie, csrf := adminSession(t, a)
	form := url.Values{"csrf": {csrf}, "id": {"0"}, "repository": {server.URL + "/owner/project"}, "provider": {"gitea"}, "mode": {"tags"}, "tag_filter_mode": {"regex"}, "tag_filter": {stableVersionRegex}}
	w := asyncAdmin(a, cookie, "POST", "/git-source", form)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, _ := a.store.Feeds()
	id := feeds[0].ID
	if feeds[0].GitTagFilterMode != "regex" {
		t.Fatal(feeds)
	}
	form.Set("id", fmt.Sprint(id))
	for _, pattern := range []string{"[", "(?=v)", "(a)\\1"} {
		form.Set("tag_filter", pattern)
		if w = asyncAdmin(a, cookie, "POST", "/git-source", form); w.Code != http.StatusBadRequest {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	feeds, _ = a.store.Feeds()
	if feeds[0].GitTagFilter != stableVersionRegex {
		t.Fatal("invalid edit changed the source")
	}
	// A failure while recalculating flags rolls back the source settings too.
	save(t, a, id, &gofeed.Item{GUID: "git:v1.2.3", Title: "old entry"})
	mustExec(t, a, "CREATE TRIGGER fail_tag_match BEFORE UPDATE OF git_tag_match ON entries BEGIN SELECT RAISE(ABORT,'failed match update'); END")
	source, _ := parseGitSource(server.URL+"/owner/project", "gitea", "tags", true)
	source.TagFilterMode = "regex"
	source.TagFilter = "nothing"
	if err := a.store.SaveGitSource(id, source); err == nil {
		t.Fatal("failed flag update succeeded")
	}
	feeds, _ = a.store.Feeds()
	if feeds[0].GitTagFilter != stableVersionRegex {
		t.Fatal("partial source save")
	}
	mustExec(t, a, "DROP TRIGGER fail_tag_match")
	source.TagFilterMode = "glob"
	source.TagFilter = "v1.*"
	if err := a.store.SaveGitSource(id, source); err != nil {
		t.Fatal(err)
	}
	rollbackRegexMigration(t, a.store.DB)
	if err := migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	feeds, _ = a.store.Feeds()
	if feeds[0].GitTagFilterMode != "glob" || feeds[0].GitTagFilter != "v1.*" {
		t.Fatal("legacy filter changed", feeds)
	}
	assertFeedAggregates(t, a)
	for _, kind := range []string{"rss", "git_repos"} {
		if _, err := normalizeSubscription(roiSource{Type: kind, TagFilterMode: "regex", URL: server.URL + "/owner", Service: "gitea"}); err == nil {
			t.Fatal("wrong source accepted regex", kind)
		}
	}
}
