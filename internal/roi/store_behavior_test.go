package roi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestOPMLAtomicityLimitsAndEscaping(t *testing.T) {
	a := testApp(t)
	for _, raw := range []string{`<wrong/>`, `<opml><body><outline xmlUrl="https://example.org/good"/><outline xmlUrl="file:///etc/passwd"/></body></opml>`, `<!DOCTYPE opml [<!ENTITY secret SYSTEM "file:///etc/passwd">]><opml><body><outline text="&secret;"/></body></opml>`, "<opml><body>" + strings.Repeat(`<outline text="deep">`, 34) + strings.Repeat(`</outline>`, 34) + "</body></opml>"} {
		if e := a.store.Import(strings.NewReader(raw)); e == nil {
			t.Fatal("invalid OPML accepted")
		}
		fs, e := a.store.Feeds()
		if e != nil || len(fs) != 0 {
			t.Fatal("partial import committed", fs, e)
		}
	}
	raw := `<opml><body><outline text="A &amp; B"><outline xmlUrl="https://example.org/feed?a=1&amp;b=2" text="&lt;script&gt;"/></outline><outline xmlUrl="https://example.org/no-title"/></body></opml>`
	if e := a.store.Import(strings.NewReader(raw)); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e := a.store.Export(&b); e != nil || strings.Contains(b.String(), "<script>") || !strings.Contains(b.String(), "A &amp; B") {
		t.Fatal(b.String(), e)
	}
	if e := a.store.Export(failingWriter{}); !errors.Is(e, io.ErrClosedPipe) {
		t.Fatal(e)
	}
}
func TestStorageFailurePaths(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := a.store.Feeds(); e == nil {
		t.Fatal("closed database read succeeded")
	}
	if e := a.store.Import(strings.NewReader(`<opml/>`)); e == nil {
		t.Fatal("closed database import succeeded")
	}
	if e := a.store.Export(io.Discard); e == nil {
		t.Fatal("closed database export succeeded")
	}
	a.config.Retention.PerFeed = 1
	if _, e := a.store.Rotate(a.config, time.Now()); e == nil {
		t.Fatal("closed database rotation succeeded")
	}
	if e := a.store.SetEnabled(id, false); e == nil {
		t.Fatal("closed database disable succeeded")
	}
	if e := a.store.DeleteFeed(id); e == nil {
		t.Fatal("closed database deletion succeeded")
	}
	if _, e := a.poller.saveEntries(context.Background(), id, &gofeed.Feed{}, time.Now()); e == nil {
		t.Fatal("closed database write succeeded")
	}
	a.poller.schedule(context.Background())
	dir := t.TempDir()
	if s, e := OpenStore(dir); e == nil {
		s.DB.Close()
		t.Fatal("opened directory as database")
	}
	if s, e := OpenStore(filepath.Join(dir, "missing", "db")); e != nil {
		t.Fatal(e)
	} else {
		s.DB.Close()
	}
	for _, raw := range []string{"feed/0", "feed/-1", "feed/1junk", "unknown", "feed/"} {
		if _, e := feedID(raw); e == nil {
			t.Fatal("invalid feed ID accepted", raw)
		}
	}
}
