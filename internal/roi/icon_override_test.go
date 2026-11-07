package roi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestIconOverridesRSSGitReaderAndROIRestore(t *testing.T) {
	a := testApp(t)
	rss := addFeed(t, a, "https://example.org/rss")
	rssAuto := "https://example.org/automatic.ico"
	mustExec(t, a, "UPDATE feeds SET icon_url=? WHERE id=?", rssAuto, rss)
	source, err := parseGitSource("https://github.com/team/project", "github", "tags", false)
	if err != nil {
		t.Fatal(err)
	}
	git := storeGit(t, a, source)
	rssManual, gitManual := "https://icons.example/rss.svg", "https://icons.example/git.png"
	cookie, csrf := adminSession(t, a)
	for _, tc := range []struct {
		id         int64
		path, icon string
	}{{rss, "/rss-settings", rssManual}, {git, "/git-source", gitManual}} {
		w := asyncAdmin(a, cookie, "POST", tc.path, url.Values{"csrf": {csrf}, "id": {fmt.Sprint(tc.id)}, "icon_override": {" " + tc.icon + " "}, "mode": {"tags"}})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	save(t, a, rss, &gofeed.Item{GUID: "entry"})
	// Other settings and syncs must retain explicit icon overrides.
	if err := a.store.SetFeedUserAgent(rss, "Custom/1"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveGitSource(git, source); err != nil {
		t.Fatal(err)
	}
	token := approvedToken(t, a)
	assertIcons := func(app *App, token string, want map[int64]string) {
		t.Helper()
		w := apiReq(app, token, "GET", "/reader/api/0/subscription/list", nil)
		var result struct {
			Subscriptions []struct{ ID, IconURL string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
			t.Fatal(w.Code, err)
		}
		if len(result.Subscriptions) != len(want) {
			t.Fatal(w.Body.String())
		}
		for _, sub := range result.Subscriptions {
			var id int64
			if _, err := fmt.Sscanf(sub.ID, "feed/%d", &id); err != nil || sub.IconURL != want[id] {
				t.Fatal(sub, err, want)
			}
		}
	}
	assertIcons(a, token, map[int64]string{rss: rssManual, git: gitManual})
	var exported bytes.Buffer
	if err := a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	if strings.Count(exported.String(), "icon_url_override:") != 2 {
		t.Fatal(exported.String())
	}
	other := testApp(t)
	if err := other.store.ImportROI(bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	feeds, err := other.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.IconOverride == "" || f.Icon != f.IconOverride {
			t.Fatal(f)
		}
	}
	if err := a.store.SetRSSSettings(rss, "", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SaveGitSource(git, source, ""); err != nil {
		t.Fatal(err)
	}
	assertIcons(a, token, map[int64]string{rss: rssAuto, git: source.Icon})
	feeds, err = a.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.IconOverride != "" {
			t.Fatal(f)
		}
	}
}

func TestIconOverrideValidationAndGitCreation(t *testing.T) {
	a := testApp(t)
	rss := addFeed(t, a, "https://example.org/rss")
	source, _ := parseGitSource("https://github.com/team/project", "github", "tags", false)
	for _, icon := range []string{"javascript:alert(1)", "data:image/svg+xml,x", "https://user:secret@example.org/icon", "https://example.org/icon#fragment", strings.Repeat("x", 8193)} {
		if err := a.store.SetRSSSettings(rss, "", nil, nil, icon); err == nil {
			t.Fatal("invalid RSS icon accepted")
		}
		if err := a.store.SaveGitSource(0, source, icon); err == nil {
			t.Fatal("invalid Git icon accepted")
		}
		raw := "format: roi\nversion: 1\nsources:\n  - type: rss\n    url: https://example.org/new\n    icon_url_override: " + icon + "\n"
		if err := a.store.ImportROI(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid imported icon accepted")
		}
	}
	if err := a.store.SaveGitSource(0, source, "https://icons.example/new.ico"); err != nil {
		t.Fatal(err)
	}
	feeds, err := a.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.GitProvider != "" && (f.Icon != "https://icons.example/new.ico" || f.IconOverride != f.Icon) {
			t.Fatal(f)
		}
	}
}
