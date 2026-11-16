package roi

import (
	"os"
	"testing"
)

// Opt-in only: credentials come from the environment and all state is temporary.
func TestGitHubGraphQLLive(t *testing.T) {
	token := os.Getenv("ROI_LIVE_GITHUB_TOKEN")
	repository := os.Getenv("ROI_LIVE_GITHUB_REPOSITORY")
	if token == "" || repository == "" {
		t.Skip("set ROI_LIVE_GITHUB_TOKEN and ROI_LIVE_GITHUB_REPOSITORY")
	}
	a := testApp(t)
	a.poller.config.Git.Tokens = map[string]string{"github.com": token}
	a.poller.config.Poll.Timeout = "45s"
	a.poller.config.Poll.MaxBytes = 32 << 20
	source, err := parseGitSource("https://github.com/"+repository, "github", "tags", false)
	if err != nil {
		t.Fatal(err)
	}
	source.TagFilter = stableVersionRegex
	if pattern := os.Getenv("ROI_LIVE_GITHUB_TAG_FILTER"); pattern != "" {
		source.TagFilter = pattern
	}
	source.TagFilterMode = "regex"
	id := storeGit(t, a, source)
	a.poller.fetch(t.Context(), id)
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 1 || feeds[0].Error != "" || feeds[0].Count == 0 || feeds[0].Count > gitItemLimit {
		t.Fatal(feeds, err)
	}
	rows, err := a.store.DB.Query("SELECT title FROM entries WHERE feed_id=? ORDER BY published DESC,id DESC LIMIT 3", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var title string
		if err = rows.Scan(&title); err != nil {
			t.Fatal(err)
		}
		titles = append(titles, title)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("fetched %d matching tags; latest: %v", feeds[0].Count, titles)
}
