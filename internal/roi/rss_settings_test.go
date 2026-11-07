package roi

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestRSSSettingsURLsIdentityAndWebsiteOverride(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "http://example.org/old")
	save(t, a, id, &gofeed.Item{GUID: "stable", Title: "Existing", Link: "https://example.org/article"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1")
	mustExec(t, a, "UPDATE feeds SET title='Custom name',site='https://example.org/automatic',etag='old',modified='old',error='HTTP 503',failures=5,paused=1,next_fetch=999")
	mustExec(t, a, "INSERT INTO folders VALUES('Folder');INSERT INTO feed_folders VALUES(?,'Folder')", id)
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "id": {fmt.Sprint(id)}, "url": {" https://example.org/new "}, "site_override": {" https://example.org/manual "}, "user_agent": {" Custom/2 "}}
	w := asyncAdmin(a, cookie, "POST", "/rss-settings", values)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 1 {
		t.Fatal(feeds, err)
	}
	f := feeds[0]
	if f.ID != id || f.URL != "https://example.org/new" || f.Site != "https://example.org/manual" || f.SiteOverride != f.Site || f.Title != "Custom name" || !f.Paused || !f.Active || f.Count != 1 || f.Unread != 0 || f.Next != 0 || f.Error != "" || f.Failures != 0 || len(f.Folders) != 1 || f.UserAgent != "Custom/2" {
		t.Fatalf("%+v", f)
	}
	var revision int64
	var etag, modified string
	if err := a.store.DB.QueryRow("SELECT git_revision,etag,modified FROM feeds WHERE id=?", id).Scan(&revision, &etag, &modified); err != nil || revision != 1 || etag != "" || modified != "" {
		t.Fatal(revision, etag, modified, err)
	}
	// Results from a request made before the edit cannot overwrite the changed subscription.
	n, err := a.poller.saveEntriesRevision(context.Background(), id, &gofeed.Feed{Link: "https://old.example", Items: []*gofeed.Item{{GUID: "stale"}}}, time.Now(), 0)
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	save(t, a, id, &gofeed.Item{GUID: "stable", Title: "Updated"})
	mustExec(t, a, "UPDATE feeds SET paused=0 WHERE id=?", id)
	if _, err = a.poller.saveEntries(context.Background(), id, &gofeed.Feed{Link: "https://wrong.example", Items: []*gofeed.Item{{GUID: "stable"}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var site string
	var count int
	var read, starred bool
	if err = a.store.DB.QueryRow("SELECT site FROM feeds WHERE id=?", id).Scan(&site); err != nil || site != "https://example.org/manual" {
		t.Fatal(site, err)
	}
	if err = a.store.DB.QueryRow("SELECT COUNT(*),is_read,starred FROM entries WHERE feed_id=?", id).Scan(&count, &read, &starred); err != nil || count != 1 || !read || !starred {
		t.Fatal(count, read, starred, err)
	}
	token := approvedToken(t, a)
	w = apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil)
	if !strings.Contains(w.Body.String(), fmt.Sprintf("feed/%d", id)) || !strings.Contains(w.Body.String(), "https://example.org/manual") {
		t.Fatal(w.Body.String())
	}
	var exported bytes.Buffer
	if err = a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	other := testApp(t)
	if err = other.store.ImportROI(bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	restored, err := other.store.Feeds()
	if err != nil || len(restored) != 1 || restored[0].SiteOverride != "https://example.org/manual" || restored[0].AutomaticSite != "https://wrong.example" {
		t.Fatal(restored, err)
	}
	values.Set("site_override", "")
	w = asyncAdmin(a, cookie, "POST", "/rss-settings", values)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err = a.poller.saveEntries(context.Background(), id, &gofeed.Feed{Link: "https://example.org/detected"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = a.store.DB.QueryRow("SELECT site FROM feeds WHERE id=?", id).Scan(&site); err != nil || site != "https://example.org/detected" {
		t.Fatal(site, err)
	}
}

func TestRSSSettingsURLValidationDuplicatesAndRollback(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "http://example.org/self")
	other := addFeed(t, a, "http://example.org/other")
	mustExec(t, a, "UPDATE feeds SET active=0 WHERE id=?", other)
	raw, website := "https://example.org/self", "https://example.org/site"
	if err := a.store.SetRSSSettings(id, "", &raw, &website); err != nil {
		t.Fatal("own HTTP/HTTPS variant rejected", err)
	}
	for _, bad := range []string{"", "ftp://example.org/rss", "https://user:pass@example.org/rss", "https://example.org/rss#bad", "https://example.org/other", strings.Repeat("a", 8193)} {
		if err := a.store.SetRSSSettings(id, "", &bad, nil); err == nil {
			t.Fatal("invalid or duplicate URL accepted", bad)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "https://user:pass@example.org", strings.Repeat("x", 8193)} {
		if err := a.store.SetRSSSettings(id, "", nil, &bad); err == nil {
			t.Fatal("invalid site accepted")
		}
	}
	if err := a.store.SetRSSSettings(id, "bad\r\nheader", nil, nil); err == nil {
		t.Fatal("header injection accepted")
	}
	mustExec(t, a, "CREATE TRIGGER fail_rss_update BEFORE UPDATE ON feeds BEGIN SELECT RAISE(FAIL,'write failed'); END")
	raw = "https://example.org/new"
	if err := a.store.SetRSSSettings(id, "new-agent", &raw, nil); err == nil {
		t.Fatal("write failure ignored")
	}
	mustExec(t, a, "DROP TRIGGER fail_rss_update")
	feeds, err := a.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.ID == id && (f.URL != "https://example.org/self" || f.SiteOverride != website || f.UserAgent != "") {
			t.Fatal(f)
		}
	}
	mustExec(t, a, "UPDATE feeds SET active=0,paused=1 WHERE id=?", id)
	if err := a.store.SetRSSSettings(id, "", &raw, nil); err != nil {
		t.Fatal(err)
	}
	var active, paused bool
	if err := a.store.DB.QueryRow("SELECT active,paused FROM feeds WHERE id=?", id).Scan(&active, &paused); err != nil || active || !paused {
		t.Fatal(active, paused, err)
	}
	for _, snippet := range []string{"type: rss\n    url: https://example.org/rss\n    site_url_override: javascript:bad", "type: git\n    repository: https://github.com/team/repo\n    service: github\n    site_url_override: https://example.org"} {
		if err := a.store.ImportROI(strings.NewReader("format: roi\nversion: 1\nsources:\n  - " + snippet + "\n")); err == nil {
			t.Fatal("invalid override import accepted")
		}
	}
	a.Close()
	if err := a.store.SetRSSSettings(id, "", nil, nil); err == nil {
		t.Fatal("closed store error ignored")
	}
}

func TestAutomaticWebsiteMigration(t *testing.T) {
	a := testApp(t)
	auto := addFeed(t, a, "https://example.org/auto")
	manual := addFeed(t, a, "https://example.org/manual")
	mustExec(t, a, "UPDATE feeds SET site='https://example.org/original',etag='cached',modified='cached',next_fetch=999")
	mustExec(t, a, "UPDATE feeds SET site='https://custom.example/',site_override='https://custom.example/' WHERE id=?", manual)
	if _, err := a.store.DB.Exec("ALTER TABLE feeds DROP COLUMN site_auto; PRAGMA user_version=18"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(a.store.DB); err != nil {
		t.Fatal(err)
	}
	feeds, err := a.store.Feeds()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range feeds {
		if f.ID == auto && f.AutomaticSite != "https://example.org/original" {
			t.Fatal(f)
		}
		if f.ID == manual && (f.AutomaticSite != "" || f.Site != "https://custom.example/" || f.Next != 0) {
			t.Fatal(f)
		}
	}
	var exported bytes.Buffer
	if err := a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	entries, err := decodeSubscriptions(bytes.NewReader(exported.Bytes()), "roi")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.URL == "https://example.org/manual" && entry.SiteURL != "" {
			t.Fatal("manual website was exported as automatic", entry)
		}
	}
	var etag string
	if err := a.store.DB.QueryRow("SELECT etag FROM feeds WHERE id=?", manual).Scan(&etag); err != nil || etag != "" {
		t.Fatal(etag, err)
	}
}
