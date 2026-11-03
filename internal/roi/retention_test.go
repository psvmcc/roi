package roi

import (
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestEntryUpdateRetentionAndTombstones(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/feed")
	items := []*gofeed.Item{{GUID: "1", Title: "one", Content: "old"}, {GUID: "2", Title: "two"}, {GUID: "3", Title: "three"}}
	if n := save(t, a, id, items...); n != 3 {
		t.Fatal(n)
	}
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1 WHERE guid='1'")
	items[0].Content = "new"
	if n := save(t, a, id, items...); n != 0 {
		t.Fatal(n)
	}
	var read, star bool
	var content string
	if e := a.store.DB.QueryRow("SELECT is_read,starred,content FROM entries WHERE guid='1'").Scan(&read, &star, &content); e != nil {
		t.Fatal(e)
	}
	if !read || !star || content != "new" {
		t.Fatal("update lost state")
	}
	mustExec(t, a, "UPDATE entries SET received=1 WHERE guid='2'")
	a.config.Retention.Months = 1
	n, e := a.store.Rotate(a.config, time.Now())
	if e != nil || n != 1 {
		t.Fatalf("rotation %d %v", n, e)
	}
	if n := save(t, a, id, items...); n != 0 {
		t.Fatal("resurrected rotated entry")
	}
	a.config.Retention.Months = 0
	a.config.Retention.PerFeed = 1
	if _, e = a.store.Rotate(a.config, time.Now()); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = a.store.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count); e != nil {
		t.Fatal(e)
	}
	if count < 1 || count > 2 {
		t.Fatal(count)
	}
}
