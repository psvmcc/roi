package roi

import (
	"strings"
	"testing"
)

func FuzzSubscriptionDocuments(f *testing.F) {
	f.Add("opml", `<opml><body><outline text="Feed" xmlUrl="https://example.org/rss"/></body></opml>`)
	f.Add("roi", "format: roi\nversion: 1\nsources:\n - type: git\n   repository: https://example.org/?p=a.git\n   service: gitweb\n   mode: tags\n")
	f.Add("opml", `<!DOCTYPE opml [<!ENTITY x SYSTEM "file:///etc/passwd">]><opml>&x;</opml>`)
	f.Fuzz(func(t *testing.T, format, body string) {
		if len(body) > 64<<10 {
			return
		}
		entries, err := decodeSubscriptions(strings.NewReader(body), format)
		if err != nil {
			return
		}
		for _, entry := range entries {
			_, _ = normalizeSubscription(entry)
		}
	})
}
func FuzzGitwebDocuments(f *testing.F) {
	f.Add(`<table class="tags"><tr><td><a href="?p=a.git;a=shortlog;h=refs/tags/v1">v1</a></td></tr></table>`)
	f.Add(`<span class="datetime">2026-10-14T13:57:49Z</span><div class="page_body"><a href="javascript:alert(1)">Unsafe</a><pre>code()</pre></div>`)
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 64<<10 {
			return
		}
		s, _ := parseGitSource("https://example.org/?p=a.git", "gitweb", "tags", false)
		_, _ = parseGitwebTags([]byte(body), s)
		message, _, _, err := parseGitwebDetail([]byte(body))
		if err != nil {
			return
		}
		html, err := gitMarkdown(message, s.URL)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(html, "<script>") {
			t.Fatal("unsafe HTML escaped the renderer")
		}
	})
}
