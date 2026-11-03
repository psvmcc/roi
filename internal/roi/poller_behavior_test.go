package roi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestNetworkAddressPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "10.1.2.3", "172.16.1.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.0.1", "240.0.0.1", "224.0.0.1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1", "::ffff:127.0.0.1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("unsafe address allowed", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("public address blocked", raw)
		}
	}
}

func TestPollRedirectErrorsRetryAndRecovery(t *testing.T) {
	a := testApp(t)
	var mode atomic.Value
	mode.Store("redirect")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "redirect":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "parse":
			_, _ = io.WriteString(w, "not a feed")
		case "retry":
			w.Header().Set("Retry-After", time.Now().Add(5*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(429)
		case "truncated":
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "<rss>")
		case "valid":
			w.Header().Set("ETag", `"ok"`)
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			_, _ = io.WriteString(w, `<rss version="2.0"><channel><title>Test</title><description>Test</description><link>https://example.org</link><item><guid>new</guid><title>New</title></item></channel></rss>`)
		}
	}))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	for _, tc := range []struct{ mode, contains string }{{"redirect", "redirect"}, {"parse", "feed"}, {"truncated", "EOF"}, {"retry", "HTTP 429"}} {
		mode.Store(tc.mode)
		a.poller.fetch(context.Background(), id)
		var msg string
		var failures int
		var next int64
		if e := a.store.DB.QueryRow("SELECT error,failures,next_fetch FROM feeds WHERE id=?", id).Scan(&msg, &failures, &next); e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(msg, tc.contains) || failures < 1 {
			t.Fatal(tc.mode, msg, failures)
		}
		if tc.mode == "retry" && next < time.Now().Add(4*time.Minute).Unix() {
			t.Fatal("HTTP-date Retry-After ignored")
		}
	}
	mode.Store("valid")
	a.poller.mu.Lock()
	a.poller.hosts = map[string]*hostState{}
	a.poller.mu.Unlock()
	a.poller.fetch(context.Background(), id)
	var msg, etag, modified string
	var failures int
	if e := a.store.DB.QueryRow("SELECT error,failures,etag,modified FROM feeds WHERE id=?", id).Scan(&msg, &failures, &etag, &modified); e != nil {
		t.Fatal(e)
	}
	if msg != "" || failures != 0 || etag == "" || modified == "" {
		t.Fatal("recovery did not reset failure state", msg, failures)
	}
}

func TestPollCancelledPausedDisabledAndTLS(t *testing.T) {
	a := testApp(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	a.poller.fetch(context.Background(), id)
	var msg string
	if e := a.store.DB.QueryRow("SELECT error FROM feeds WHERE id=?", id).Scan(&msg); e != nil || !strings.Contains(msg, "certificate") {
		t.Fatal(msg, e)
	}
	before := countRows(t, a, "fetches", id)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.poller.fetch(ctx, id)
	mustExec(t, a, "UPDATE feeds SET paused=1 WHERE id=?", id)
	a.poller.fetch(context.Background(), id)
	if e := a.store.SetEnabled(id, false); e != nil {
		t.Fatal(e)
	}
	a.poller.fetch(context.Background(), id)
	if n := countRows(t, a, "fetches", id); n != before {
		t.Fatal("inactive fetch created history")
	}
	a.poller.Cancel(999)
}

func TestEntryFallbackIDsDatesAndStableOrder(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	updated := at.Add(-time.Hour)
	published := at.Add(-2 * time.Hour)
	items := []*gofeed.Item{{Title: "Latest", Link: "https://example.org/latest", Description: "description", UpdatedParsed: &updated, Author: &gofeed.Person{Name: "Author"}}, {Title: "Old", Published: "raw date", PublishedParsed: &published}}
	f := &gofeed.Feed{Title: "Source", Link: "https://example.org", Items: items}
	n, e := a.poller.saveEntries(context.Background(), id, f, at)
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	n, e = a.poller.saveEntries(context.Background(), id, f, at.Add(time.Hour))
	if e != nil || n != 0 {
		t.Fatal("fallback IDs not stable", n, e)
	}
	var guid, content, author string
	var date int64
	if e = a.store.DB.QueryRow("SELECT guid,content,author,published FROM entries ORDER BY id DESC LIMIT 1").Scan(&guid, &content, &author, &date); e != nil {
		t.Fatal(e)
	}
	if guid != "https://example.org/latest" || content != "description" || author != "Author" || date != updated.Unix() {
		t.Fatal(guid, content, author, date)
	}
	if e = a.store.DB.QueryRow("SELECT guid FROM entries ORDER BY id LIMIT 1").Scan(&guid); e != nil || !strings.HasPrefix(guid, "sha256:") {
		t.Fatal(guid, e)
	}
	other := addFeed(t, a, "https://example.org/other")
	if n := save(t, a, other, items...); n != 2 {
		t.Fatal("GUIDs incorrectly global", n)
	}
}

func TestRetentionAgeCountStarredAndBatching(t *testing.T) {
	for _, tc := range []struct {
		name          string
		months, count int
		keep          bool
		want          int
	}{{"unlimited", 0, 0, true, 4}, {"age", 1, 0, false, 2}, {"count", 0, 2, false, 2}, {"both", 1, 1, false, 1}, {"protected", 1, 1, true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			id := addFeed(t, a, "https://example.org/rss")
			for i := range 4 {
				save(t, a, id, &gofeed.Item{GUID: fmt.Sprint(i)})
			}
			mustExec(t, a, "UPDATE entries SET received=? WHERE guid IN ('0','1')", time.Now().AddDate(0, -2, 0).UnixMicro())
			mustExec(t, a, "UPDATE entries SET starred=1 WHERE guid='0'")
			a.config.Retention.Months = tc.months
			a.config.Retention.PerFeed = tc.count
			a.config.Retention.KeepStarred = tc.keep
			if _, e := a.store.Rotate(a.config, time.Now()); e != nil {
				t.Fatal(e)
			}
			if got := countRows(t, a, "entries", id); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/batch")
	mustExec(t, a, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<1005) INSERT INTO entries(feed_id,guid,title,url,content,author,published,received,updated) SELECT ?,CAST(x AS TEXT),'','','','',1,x,1 FROM n`, id)
	a.config.Retention.PerFeed = 1
	n, e := a.store.Rotate(a.config, time.Now())
	if e != nil || n != 1000 {
		t.Fatal(n, e)
	}
	n, e = a.store.Rotate(a.config, time.Now())
	if e != nil || n != 4 {
		t.Fatal(n, e)
	}
	if countRows(t, a, "entries", id) != 1 || countRows(t, a, "tombstones", id) != 1004 {
		t.Fatal("retention batching incorrect")
	}
}
