package roi

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOPMLRoundTrip(t *testing.T) {
	a := testApp(t)
	raw := `<opml version="2.0"><body><outline text="Dev"><outline text="RSS" xmlUrl="https://example.org/rss"/><outline text="Inner"><outline text="Atom" xmlUrl="https://example.org/atom"/></outline></outline></body></opml>`
	for range 2 {
		if e := a.store.Import(strings.NewReader(raw)); e != nil {
			t.Fatal(e)
		}
	}
	fs, e := a.store.Feeds()
	if e != nil {
		t.Fatal(e)
	}
	if len(fs) != 2 {
		t.Fatalf("duplicate feeds: %d", len(fs))
	}
	var out bytes.Buffer
	if e = a.store.Export(&out); e != nil {
		t.Fatal(e)
	}
	b := testApp(t)
	if e = b.store.Import(&out); e != nil {
		t.Fatal(e)
	}
	fs, e = b.store.Feeds()
	if e != nil {
		t.Fatal(e)
	}
	if len(fs) != 2 {
		t.Fatal(fs)
	}
	folders := map[string]bool{}
	for _, f := range fs {
		for _, g := range f.Folders {
			folders[g] = true
		}
	}
	if !folders["Dev"] || !folders["Dev/Inner"] {
		t.Fatal(folders)
	}
	if e = a.store.Import(strings.NewReader(`<opml><body><outline xmlUrl="file:///etc/passwd"/></body></opml>`)); e == nil {
		t.Fatal("accepted file URL")
	}
}

func TestExampleOPML(t *testing.T) {
	paths, e := filepath.Glob("../../*.opml")
	if e != nil {
		t.Fatal(e)
	}
	if len(paths) == 0 {
		t.Skip("user OPML not present")
	}
	a := testApp(t)
	f, e := os.Open(paths[0])
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = a.store.Import(f); e != nil {
		t.Fatal(e)
	}
	fs, e := a.store.Feeds()
	if e != nil {
		t.Fatal(e)
	}
	if len(fs) < 10 {
		t.Fatalf("unexpected subscription count: %d", len(fs))
	}
	t.Logf("imported %d feeds", len(fs))
}

func TestOPMLDuplicateURLsAcrossFolders(t *testing.T) {
	a := testApp(t)
	first := `<opml><body><outline text="Original"><outline title="Original title" xmlUrl="https://example.org/feed" htmlUrl="https://example.org"/></outline></body></opml>`
	if e := a.store.Import(strings.NewReader(first)); e != nil {
		t.Fatal(e)
	}
	mustExec(t, a, "UPDATE feeds SET active=0,paused=1")
	other := `<opml><body><outline text="Elsewhere"><outline title="Changed title" xmlUrl="https://example.org/feed" htmlUrl="https://other.org"/><outline title="New" xmlUrl="https://example.org/new"/></outline><outline text="Third"><outline title="Duplicate" xmlUrl="https://example.org/new"/></outline></body></opml>`
	if e := a.store.Import(strings.NewReader(other)); e != nil {
		t.Fatal(e)
	}
	fs, e := a.store.Feeds()
	if e != nil {
		t.Fatal(e)
	}
	if len(fs) != 2 {
		t.Fatalf("got %d feeds", len(fs))
	}
	for _, f := range fs {
		switch f.URL {
		case "https://example.org/feed":
			if f.Title != "Original title" || f.Site != "https://example.org" || f.Active || !f.Paused || len(f.Folders) != 1 || f.Folders[0] != "Original" {
				t.Fatalf("existing subscription changed: %+v", f)
			}
		case "https://example.org/new":
			if f.Title != "New" || len(f.Folders) != 1 || f.Folders[0] != "Elsewhere" {
				t.Fatalf("duplicate folder added: %+v", f)
			}
		}
	}
}
