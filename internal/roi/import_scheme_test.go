package roi

import (
	"fmt"
	"strings"
	"testing"
)

func importSchemeFixture(format, kind, raw string) string {
	if format == "roi" {
		if kind == "git" {
			return fmt.Sprintf("format: roi\nversion: 1\nsources:\n  - type: git\n    repository: %s\n    service: github\n    title: Imported\n    mode: tags\n", raw)
		}
		return fmt.Sprintf("format: roi\nversion: 1\nsources:\n  - type: rss\n    url: %s\n    title: Imported\n    folders: [Other]\n    user_agent: Imported/1\n", raw)
	}
	if kind == "git" {
		return fmt.Sprintf(`<opml><body><outline text="GIT_Releases"><outline type="git" text="Imported" xmlUrl="%s" roiProvider="github" roiMode="tags"/></outline></body></opml>`, raw)
	}
	return fmt.Sprintf(`<opml><body><outline text="Other"><outline text="Imported" xmlUrl="%s"/></outline></body></opml>`, raw)
}
func importSchemeFile(a *App, format, raw string) error {
	if format == "roi" {
		return a.store.ImportROI(strings.NewReader(raw))
	}
	return a.store.Import(strings.NewReader(raw))
}

func TestImportHTTPHTTPSExistingSubscriptions(t *testing.T) {
	for _, format := range []string{"opml", "roi"} {
		for _, existingScheme := range []string{"http", "https"} {
			for _, existingKind := range []string{"rss", "git"} {
				for _, importedKind := range []string{"rss", "git"} {
					t.Run(strings.Join([]string{format, existingScheme, existingKind, importedKind}, "/"), func(t *testing.T) {
						a := testApp(t)
						existingURL := existingScheme + "://github.com/team/repo"
						var id int64
						if existingKind == "git" {
							source, _ := parseGitSource(existingURL, "github", "releases", false)
							id = storeGit(t, a, source)
						} else {
							id = addFeed(t, a, existingURL)
							mustExec(t, a, "INSERT INTO folders(name) VALUES('Original')")
							mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Original')", id)
						}
						mustExec(t, a, "UPDATE feeds SET title='Original',active=0 WHERE id=?", id)
						incomingScheme := "https"
						if existingScheme == "https" {
							incomingScheme = "http"
						}
						raw := importSchemeFixture(format, importedKind, incomingScheme+"://github.com/team/repo")
						if err := importSchemeFile(a, format, raw); err != nil {
							t.Fatal(err)
						}
						feeds, err := a.store.Feeds()
						if err != nil || len(feeds) != 1 {
							t.Fatal(feeds, err)
						}
						f := feeds[0]
						if f.ID != id || f.URL != existingURL || f.Title != "Original" || f.Active || f.UserAgent != "" {
							t.Fatal("import changed existing subscription", f)
						}
						wantFolder := "Original"
						if existingKind == "git" {
							wantFolder = gitFolder
						}
						if len(f.Folders) != 1 || f.Folders[0] != wantFolder {
							t.Fatal(f)
						}
						if (f.GitProvider != "") != (existingKind == "git") {
							t.Fatal("source type changed", f)
						}
					})
				}
			}
		}
	}
}

func TestImportHTTPHTTPSWithinFileAndDistinctURLs(t *testing.T) {
	for _, format := range []string{"opml", "roi"} {
		for _, kind := range []string{"rss", "git"} {
			t.Run(format+"/"+kind, func(t *testing.T) {
				a := testApp(t)
				first := importSchemeFixture(format, kind, "http://github.com/team/repo")
				second := importSchemeFixture(format, kind, "https://github.com/team/repo")
				distinct := importSchemeFixture(format, kind, "https://github.com/team/other")
				var combined string
				if format == "roi" {
					combined = first + strings.SplitN(second, "sources:\n", 2)[1] + strings.SplitN(distinct, "sources:\n", 2)[1]
				} else {
					combined = strings.Replace(first, "</body></opml>", "", 1) + strings.TrimPrefix(second, "<opml><body>")
					combined = strings.Replace(combined, "</body></opml>", "", 1) + strings.TrimPrefix(distinct, "<opml><body>")
				}
				if err := importSchemeFile(a, format, combined); err != nil {
					t.Fatal(err)
				}
				feeds, err := a.store.Feeds()
				if err != nil || len(feeds) != 2 {
					t.Fatal(feeds, err)
				}
				for _, f := range feeds {
					if f.URL == "https://github.com/team/repo" {
						t.Fatal("first occurrence did not win", f)
					}
				}
			})
		}
	}
	a := testApp(t)
	raw := "format: roi\nversion: 1\nsources:\n"
	for _, url := range []string{"http://example.org/feed?a=1", "https://example.org/feed?a=1", "https://example.org/feed?a=2", "https://example.org/other?a=1", "https://example.org:8443/feed?a=1"} {
		raw += fmt.Sprintf("  - type: rss\n    url: %s\n", url)
	}
	if err := a.store.ImportROI(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 4 {
		t.Fatal("distinct path/query/port conflated", feeds, err)
	}
}
