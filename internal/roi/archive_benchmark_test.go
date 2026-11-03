package roi

import (
	"path/filepath"
	"strings"
	"testing"
)

// Setup is intentionally outside the timed section: metadata requests must not
// read article bodies even when the archive contains tens of megabytes of text.
func BenchmarkLargeArchiveMetadata(b *testing.B) {
	s, err := OpenStore(filepath.Join(b.TempDir(), "archive.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.DB.Close()
	if _, err = s.DB.Exec("INSERT INTO feeds(url,title) VALUES('https://example.org/rss','Archive')"); err != nil {
		b.Fatal(err)
	}
	if _, err = s.DB.Exec(`WITH RECURSIVE numbers(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM numbers WHERE x<10000) INSERT INTO entries(feed_id,guid,title,url,content,author,published,received,updated) SELECT 1,CAST(x AS TEXT),'Article','https://example.org/',?,'Author',x,x,x FROM numbers`, strings.Repeat("x", 4096)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		feeds, err := s.Feeds()
		if err != nil || len(feeds) != 1 || feeds[0].Count != 10000 {
			b.Fatal(feeds, err)
		}
	}
}
