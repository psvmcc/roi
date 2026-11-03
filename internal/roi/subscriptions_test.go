package roi

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestROIRoundTripStatesAndRSSOnlyOPML(t *testing.T) {
	a := testApp(t)
	addFeed(t, a, "https://rss.example/news.xml")
	first, _ := parseGitSource("https://git.example/team/one", "gitea", "tags", true)
	first.Icon = "https://git.example/assets/icon.svg"
	one := storeGit(t, a, first)
	second, _ := parseGitSource("https://gitlab.example/group/sub/two", "gitlab", "releases", false)
	two := storeGit(t, a, second)
	mustExec(t, a, "UPDATE feeds SET title='Custom Git name',paused=1 WHERE id=?", one)
	if err := a.store.SetEnabled(two, false); err != nil {
		t.Fatal(err)
	}
	a.config.Git.Tokens = map[string]string{"git.example": "private-api-token"}
	var exported, opml strings.Builder
	if err := a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), "private-api-token") || !strings.Contains(exported.String(), "rss.example") {
		t.Fatal("ROI leaked credentials or omitted RSS")
	}
	if err := a.store.Export(&opml); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opml.String(), "git.example") || !strings.Contains(opml.String(), "rss.example") {
		t.Fatal("OPML is not RSS-only", opml.String())
	}
	other := testApp(t)
	if err := other.store.ImportROI(strings.NewReader(exported.String())); err != nil {
		t.Fatal(err)
	}
	feeds, err := other.store.Feeds()
	if err != nil || len(feeds) != 3 {
		t.Fatal(feeds, err)
	}
	for _, f := range feeds {
		if f.GitProvider == "" {
			if f.URL != "https://rss.example/news.xml" || !f.Active {
				t.Fatal(f)
			}
			continue
		}
		if len(f.Folders) != 1 || f.Folders[0] != gitFolder {
			t.Fatal("Git directory missing", f)
		}
		if f.URL == first.URL {
			if f.Title != "Custom Git name" || !f.Paused || !f.Active || !f.GitPreview || f.GitMode != "tags" || f.Icon != first.Icon {
				t.Fatal(f)
			}
		} else if f.Active || f.Paused || f.GitPreview || f.GitProvider != "gitlab" {
			t.Fatal(f)
		}
	}
	// A repeated import cannot modify settings or enable a known subscription.
	changed := strings.ReplaceAll(exported.String(), "enabled: false", "enabled: true")
	if err := other.store.ImportROI(strings.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	feeds, _ = other.store.Feeds()
	if len(feeds) != 3 {
		t.Fatal("duplicate source")
	}
	for _, f := range feeds {
		if f.URL == second.URL && f.Active {
			t.Fatal("known source re-enabled")
		}
	}
}

func TestROIDefaultsDeduplicationAndValidation(t *testing.T) {
	a := testApp(t)
	addFeed(t, a, "https://github.com/team/existing")
	valid := `format: roi
version: 1
sources:
  - type: git
    repository: https://github.com/team/existing
    service: github
  - type: git
    repository: https://github.com/team/new.git/
    service: github
    mode: tags
    include_preview: true
  - type: git
    repository: https://github.com/team/new
    service: github
    mode: releases
`
	if err := a.store.ImportROI(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	feeds, _ := a.store.Feeds()
	if len(feeds) != 2 {
		t.Fatal(feeds)
	}
	for _, f := range feeds {
		if f.URL == "https://github.com/team/new" {
			if f.GitMode != "tags" || !f.GitPreview || !f.Active || f.Title != "github.com :: team/new" {
				t.Fatal(f)
			}
		} else if f.GitProvider != "" {
			t.Fatal("RSS source converted")
		}
	}
	base := "format: roi\nversion: 1\nsources:\n  - type: git\n    repository: https://github.com/team/repo\n    service: github\n"
	for _, bad := range []string{"", "[", strings.Replace(base, "version: 1", "version: 2", 1), strings.Replace(base, "roi", "other", 1), base + "---\n{}\n", base + "unknown: true\n", base + "    token: forbidden\n", strings.Replace(base, "service: github", "service: auto", 1), strings.Replace(base, "service: github", "service: other", 1), base + "    mode: bad\n", base + "    icon_url: javascript:bad\n", base + "    title: \"line\\nbreak\"\n", base + "    title: " + strings.Repeat("a", 513) + "\n", strings.Repeat("x", maxROIBytes+1), "format: roi\nversion: 1\nsources:\n" + strings.Repeat("  - {}\n", 10001)} {
		if err := a.store.ImportROI(strings.NewReader(bad)); err == nil {
			t.Fatal("invalid ROI accepted", bad[:min(len(bad), 100)])
		}
	}
	if err := a.store.ImportROI(strings.NewReader("format: roi\nversion: 1\nsources: []\n")); err != nil {
		t.Fatal(err)
	}
	if err := a.store.ImportROI(failedROIReader{}); err == nil {
		t.Fatal("reader failure ignored")
	}
}

type failedROIReader struct{}

func (failedROIReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type failedROIWriter struct{}

func (failedROIWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestROITransactionalFailuresAndExample(t *testing.T) {
	raw, err := os.ReadFile("../../examples/subscriptions.roi")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"feeds", "folders", "feed_folders"} {
		t.Run(table, func(t *testing.T) {
			a := testApp(t)
			mustExec(t, a, "CREATE TRIGGER reject_roi BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'failure'); END")
			if err := a.store.ImportROI(bytes.NewReader(raw)); err == nil {
				t.Fatal("failed import succeeded")
			}
			var count int
			a.store.DB.QueryRow("SELECT count(*) FROM feeds").Scan(&count)
			if count != 0 {
				t.Fatal("partial import committed")
			}
		})
	}
	a := testApp(t)
	if err := a.store.ImportROI(bytes.NewReader(raw)); err != nil {
		t.Fatal("example invalid", err)
	}
	if err := a.store.ExportROI(failedROIWriter{}); err == nil {
		t.Fatal("write error ignored")
	}
	a.Close()
	if err := a.store.ImportROI(bytes.NewReader(raw)); err == nil {
		t.Fatal("closed store import succeeded")
	}
	if err := a.store.ExportROI(io.Discard); err == nil {
		t.Fatal("closed store export succeeded")
	}
	// Previous ROI Git OPML files remain importable, though new exports use ROI.
	a = testApp(t)
	old := `<opml><body><outline text="GIT_Releases"><outline type="git" text="Legacy Git" xmlUrl="https://github.com/team/legacy" roiProvider="github" roiMode="tags"/></outline></body></opml>`
	if err := a.store.Import(strings.NewReader(old)); err != nil {
		t.Fatal("legacy OPML import broken", err)
	}
}

func uploadROI(a *App, cookie *http.Cookie, csrf, raw string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mp := multipart.NewWriter(&body)
	_ = mp.WriteField("csrf", csrf)
	part, _ := mp.CreateFormFile("roi", "subscriptions.roi")
	_, _ = io.WriteString(part, raw)
	_ = mp.Close()
	r := httptest.NewRequest("POST", "/import-roi", &body)
	r.Header.Set("Content-Type", mp.FormDataContentType())
	r.Header.Set("Accept", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	return w
}
func TestROIAdminImportExportAndSplitTables(t *testing.T) {
	a := testApp(t)
	rss := addFeed(t, a, "https://rss.example/news")
	cookie, csrf := adminSession(t, a)
	raw, err := os.ReadFile("../../examples/subscriptions.roi")
	if err != nil {
		t.Fatal(err)
	}
	if w := uploadROI(a, nil, csrf, string(raw)); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := uploadROI(a, cookie, "wrong", string(raw)); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := uploadROI(a, cookie, csrf, "bad"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, cookie, "POST", "/import-roi", url.Values{"csrf": {csrf}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	for range 2 {
		if w := uploadROI(a, cookie, csrf, string(raw)); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := adminRequest(a, cookie, "GET", "/export-roi", nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), ".roi") || !strings.Contains(w.Header().Get("Content-Type"), "yaml") {
		t.Fatal(w.Code, w.Header())
	}
	for _, path := range []string{"/", "/?fragment=1"} {
		body := adminRequest(a, cookie, "GET", path, nil).Body.String()
		rssTable := strings.Split(strings.Split(body, `aria-label="RSS Feeds"`)[1], "</table>")[0]
		gitTable := strings.Split(strings.Split(body, `aria-label="GIT Releases Feeds"`)[1], "</table>")[0]
		if !strings.Contains(rssTable, "rss.example") || strings.Contains(rssTable, "nginx/nginx") || !strings.Contains(gitTable, "nginx/nginx") || strings.Contains(gitTable, "rss.example") || strings.Contains(gitTable, "go.dev") || !strings.Contains(rssTable, "go.dev") {
			t.Fatal("feed types mixed")
		}
		if !strings.Contains(body, `id="git-feed-list"`) {
			t.Fatal("Git fragment missing")
		}
	}
	var gitID int64
	a.store.DB.QueryRow("SELECT id FROM feeds WHERE git_provider<>''").Scan(&gitID)
	for _, kind := range []string{"rss", "git"} {
		mustExec(t, a, "UPDATE feeds SET next_fetch=9999999999")
		w := asyncAdmin(a, cookie, "POST", "/sync", url.Values{"csrf": {csrf}, "id": {"0"}, "kind": {kind}})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var rssNext, gitNext int64
		a.store.DB.QueryRow("SELECT next_fetch FROM feeds WHERE id=?", rss).Scan(&rssNext)
		a.store.DB.QueryRow("SELECT next_fetch FROM feeds WHERE id=?", gitID).Scan(&gitNext)
		if kind == "rss" && (rssNext != 0 || gitNext == 0) || kind == "git" && (gitNext != 0 || rssNext == 0) {
			t.Fatal("sync scope incorrect", kind, rssNext, gitNext)
		}
	}
	if w := asyncAdmin(a, cookie, "POST", "/sync", url.Values{"csrf": {csrf}, "id": {"0"}, "kind": {"other"}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	// The shared disabled-only filter applies separately to both tables.
	if err := a.store.SetEnabled(gitID, false); err != nil {
		t.Fatal(err)
	}
	body := adminRequest(a, cookie, "GET", "/?disabled=1", nil).Body.String()
	if !strings.Contains(body, fmt.Sprintf(`data-feed-id="%d"`, gitID)) || strings.Contains(body, fmt.Sprintf(`data-feed-id="%d"`, rss)) {
		t.Fatal("disabled filter mixed types")
	}
}

func TestROIRSSSettingsRoundTripAndDeduplication(t *testing.T) {
	a := testApp(t)
	raw := `format: roi
version: 1
sources:
  - type: rss
    url: https://rss.example/paused
    title: Custom RSS
    site_url: https://rss.example/
    icon_url: https://rss.example/icon.png
    folders: [Development/Go, Favorites, Favorites]
    paused: true
  - type: rss
    url: https://rss.example/disabled
    enabled: false
  - type: rss
    url: https://rss.example/plain
  - type: rss
    url: https://rss.example/paused
    title: Duplicate
    folders: [Wrong]
  - type: git
    repository: https://github.com/team/repo
    service: github
  - type: rss
    url: https://github.com/team/repo
    title: Cannot convert Git
`
	if err := a.store.ImportROI(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	var exported strings.Builder
	if err := a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err := other.store.ImportROI(strings.NewReader(exported.String())); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*Store{a.store, other.store} {
		feeds, err := store.Feeds()
		if err != nil || len(feeds) != 4 {
			t.Fatal(feeds, err)
		}
		for _, f := range feeds {
			switch f.URL {
			case "https://rss.example/paused":
				if f.Title != "Custom RSS" || !f.Active || !f.Paused || f.Site != "https://rss.example/" || f.Icon != "https://rss.example/icon.png" || strings.Join(f.Folders, ",") != "Development/Go,Favorites" {
					t.Fatal(f)
				}
			case "https://rss.example/disabled":
				if f.Active || f.Paused || f.Title != f.URL || len(f.Folders) != 0 {
					t.Fatal(f)
				}
			case "https://rss.example/plain":
				if !f.Active || f.Paused || f.Title != f.URL || len(f.Folders) != 0 {
					t.Fatal(f)
				}
			case "https://github.com/team/repo":
				if f.GitProvider != "github" || f.Title == "Cannot convert Git" || len(f.Folders) != 1 || f.Folders[0] != gitFolder {
					t.Fatal(f)
				}
			}
		}
	}
	// A second import must preserve folder membership, name and disabled state.
	changed := strings.ReplaceAll(strings.ReplaceAll(exported.String(), "enabled: false", "enabled: true"), "Custom RSS", "Renamed")
	if err := other.store.ImportROI(strings.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	feeds, _ := other.store.Feeds()
	for _, f := range feeds {
		if f.Title == "Renamed" || f.URL == "https://rss.example/disabled" && f.Active {
			t.Fatal(f)
		}
	}
	var count int
	if err := other.store.DB.QueryRow("SELECT count(*) FROM folders WHERE name='Wrong'").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestROIRejectsOldFormatAndInvalidRSSAtomically(t *testing.T) {
	a := testApp(t)
	prefix := "format: roi\nversion: 1\nsources:\n  - type: rss\n    url: https://valid.example/rss\n"
	badSources := []string{
		"  - url: https://example.org/rss\n",
		"  - type: other\n    url: https://example.org/rss\n",
		"  - type: rss\n    url: javascript:bad\n",
		"  - type: rss\n    url: https://example.org/rss\n    site_url: javascript:bad\n",
		"  - type: rss\n    url: https://example.org/rss\n    icon_url: javascript:bad\n",
		"  - type: rss\n    url: https://example.org/rss\n    folders: [GIT_Releases]\n",
		"  - type: rss\n    url: https://example.org/rss\n    folders: [GIT_Releases/Sub]\n",
		"  - type: rss\n    url: https://example.org/rss\n    folders: [' ']\n",
		"  - type: rss\n    url: https://example.org/rss\n    folders: [\"line\\nbreak\"]\n",
		"  - type: rss\n    url: https://example.org/rss\n    folders: ['" + strings.Repeat("a", 513) + "']\n",
		"  - type: rss\n    url: https://example.org/rss\n    service: github\n",
		"  - type: rss\n    url: https://example.org/rss\n    repository: https://github.com/team/repo\n",
		"  - type: rss\n    url: https://example.org/rss\n    mode: tags\n",
		"  - type: rss\n    url: https://example.org/rss\n    include_preview: true\n",
		"  - type: git\n    repository: https://github.com/team/repo\n    service: github\n    url: https://example.org/rss\n",
		"  - type: git\n    repository: https://github.com/team/repo\n    service: github\n    folders: [Favorites]\n",
	}
	for _, bad := range badSources {
		if err := a.store.ImportROI(strings.NewReader(prefix + bad)); err == nil {
			t.Fatal("invalid source accepted", bad)
		}
		var count int
		if err := a.store.DB.QueryRow("SELECT count(*) FROM feeds").Scan(&count); err != nil || count != 0 {
			t.Fatal("partial import", count, err)
		}
	}
	legacy := "format: roi-git\nversion: 1\nsources:\n  - repository: https://github.com/team/repo\n    service: github\n"
	if err := a.store.ImportROI(strings.NewReader(legacy)); err == nil {
		t.Fatal("legacy ROIG format accepted")
	}
}

func TestSubscriptionDownloadTimestamps(t *testing.T) {
	a := testApp(t)
	cookie, _ := adminSession(t, a)
	for route, extension := range map[string]string{"/export-roi": ".roi", "/export": ".opml"} {
		before := time.Now().UTC()
		w := adminRequest(a, cookie, "GET", route, nil)
		media, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if w.Code != 200 || err != nil || media != "attachment" {
			t.Fatal(w.Code, w.Header(), err)
		}
		name := params["filename"]
		if !strings.HasPrefix(name, "roi-") || !strings.HasSuffix(name, extension) {
			t.Fatal(name)
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "roi-"), extension)
		parsed, err := time.Parse("20060102T150405.000000000Z", stamp)
		if err != nil || parsed.Before(before) || parsed.After(time.Now().UTC()) {
			t.Fatal(name, err)
		}
	}
	if w := adminRequest(a, cookie, "GET", "/export-roig", nil); w.Code != 404 {
		t.Fatal("old export route retained", w.Code)
	}
}
