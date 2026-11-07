package roi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestRSSAutomaticIconDiscoveryOverrideAnd304(t *testing.T) {
	var address string
	var metadata atomic.Bool
	metadata.Store(true)
	var unchanged atomic.Bool
	var pageCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			if unchanged.Load() || (metadata.Load() && r.Header.Get("If-None-Match") == "version") {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			image := ""
			if metadata.Load() {
				image = "<image><url>/images/feed.svg</url><title>Feed</title><link>" + address + "/site</link></image>"
			}
			w.Header().Set("Content-Type", "application/rss+xml")
			w.Header().Set("ETag", "version")
			fmt.Fprintf(w, `<rss version="2.0"><channel><title>Feed</title><link>%s/site</link><description>Feed</description>%s<item><guid>stable</guid><title>Article</title></item></channel></rss>`, address, image)
			return
		}
		if r.URL.Path == "/site" {
			pageCalls.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><link rel="shortcut icon" href="/assets/site.ico"></head></html>`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	address = srv.URL
	a := testApp(t)
	a.poller.findSiteIcon = a.poller.fetchSiteIcon
	id := addFeed(t, a, address+"/feed")
	if err := a.store.SetRSSSettings(id, "IconTest/1", nil, nil, "https://custom.example/manual.svg"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE feeds SET etag='version',modified='old' WHERE id=?", id)
	a.poller.fetch(t.Context(), id)
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 1 {
		t.Fatal(feeds, err)
	}
	if feeds[0].Icon != "https://custom.example/manual.svg" || feeds[0].AutomaticIcon != address+"/images/feed.svg" || pageCalls.Load() != 0 {
		t.Fatal(feeds[0], pageCalls.Load())
	}
	if err := a.store.SetRSSSettings(id, "IconTest/1", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	feeds, _ = a.store.Feeds()
	if feeds[0].Icon != address+"/images/feed.svg" {
		t.Fatal("automatic icon was not restored", feeds[0])
	}
	// A 304 response must not replace a metadata icon with a lower-priority favicon.
	mustExec(t, a, "UPDATE feeds SET icon_checked=0 WHERE id=?", id)
	unchanged.Store(true)
	a.poller.fetch(t.Context(), id)
	feeds, _ = a.store.Feeds()
	if feeds[0].Icon != address+"/images/feed.svg" || pageCalls.Load() != 0 {
		t.Fatal(feeds, pageCalls.Load())
	}
	unchanged.Store(false)
	metadata.Store(false)
	mustExec(t, a, "UPDATE feeds SET icon_checked=0 WHERE id=?", id)
	a.poller.fetch(t.Context(), id)
	feeds, _ = a.store.Feeds()
	if feeds[0].Icon != address+"/assets/site.ico" || pageCalls.Load() != 1 || feeds[0].Count != 1 || feeds[0].Error != "" {
		t.Fatal(feeds[0], pageCalls.Load())
	}
	a.poller.fetch(t.Context(), id)
	if pageCalls.Load() != 1 {
		t.Fatal("website fetched on every sync")
	}
}

func TestRSSAtomIconMetadata(t *testing.T) {
	var address string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		fmt.Fprintf(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><id>atom</id><link href="%s/"/><icon>/atom.ico</icon><entry><id>one</id><title>Entry</title></entry></feed>`, address)
	}))
	defer srv.Close()
	address = srv.URL
	a := testApp(t)
	id := addFeed(t, a, address+"/atom")
	a.poller.fetch(t.Context(), id)
	feeds, err := a.store.Feeds()
	if err != nil || feeds[0].Icon != address+"/atom.ico" {
		t.Fatal(feeds, err)
	}
}

func TestSiteIconHTMLReferencesAndFallbacks(t *testing.T) {
	cases := []struct{ html, want string }{
		{`<link rel="icon" href="/favicon.svg">`, "https://example.org/favicon.svg"},
		{`<base href="/assets/"><link rel="apple-touch-icon" href="touch.png"><link rel="shortcut ICON" href="small.svg">`, "https://example.org/assets/small.svg"},
		{`<link rel="icon" href="data:image/png,x"><link rel="apple-touch-icon" href="//cdn.example/icon.png">`, "https://cdn.example/icon.png"},
		{`<base href="javascript:bad"><link rel="icon" href="../icon.ico">`, "https://example.org/icon.ico"},
		{`<base href="/one/"><base href="/two/"><link rel="icon" href="a.ico">`, "https://example.org/one/a.ico"},
		{`<link rel="icon" href="https://user:secret@example.org/x"><link rel="icon" href="%zz">`, ""},
		{`<head></head><body><link rel="icon" href="/body.ico"></body>`, ""},
	}
	for _, tc := range cases {
		if got := htmlSiteIcon([]byte(tc.html), "https://example.org/news/page"); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
	a := testApp(t)
	calls := 0
	a.poller.findSiteIcon = func(context.Context, string, string) (string, error) { calls++; return "", errors.New("offline") }
	feed := &gofeed.Feed{Link: "https://site.example/news", Image: &gofeed.Image{URL: "javascript:bad"}}
	icon := a.poller.rssIcon(t.Context(), feed, "https://feeds.example/rss", "", "", "", "", 0, "Agent")
	if icon.URL != "https://site.example/favicon.ico" || calls != 1 {
		t.Fatal(icon, calls)
	}
	icon = a.poller.rssIcon(t.Context(), feed, "https://feeds.example/rss", "", "", "https://cdn.example/old.ico", feed.Link, 0, "Agent")
	if icon.URL != "https://cdn.example/old.ico" || calls != 2 {
		t.Fatal(icon, calls)
	}
	icon = a.poller.rssIcon(t.Context(), feed, "https://feeds.example/rss", "", "", "https://cdn.example/legacy.ico", "", 0, "Agent")
	if icon.URL != "https://cdn.example/legacy.ico" || calls != 2 {
		t.Fatal(icon, calls)
	}
	icon = a.poller.rssIcon(t.Context(), nil, "https://feeds.example/path/rss?x=1", "", "", "", "", 0, "Agent")
	if icon.URL != "https://feeds.example/favicon.ico" {
		t.Fatal(icon)
	}
	icon = a.poller.rssIcon(t.Context(), nil, "%zz", "", "", "saved", "site", 5, "Agent")
	if icon.URL != "saved" || icon.Checked != 5 {
		t.Fatal(icon)
	}
	a.poller.findSiteIcon = func(context.Context, string, string) (string, error) { return "", nil }
	icon = a.poller.rssIcon(t.Context(), feed, "https://feeds.example/rss", "", "https://override.example/", "", "", 0, "Agent")
	if icon.URL != "https://override.example/favicon.ico" {
		t.Fatal(icon)
	}
	if resolvedIcon("https://example.org/", strings.Repeat("x", 8193)) != "" || resolvedIcon("%zz", "/x") != "" {
		t.Fatal("invalid icon resolved")
	}
}

func TestSiteIconHTTPBoundsRedirectsAndNetworkPolicy(t *testing.T) {
	var slow atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/site", http.StatusFound)
		case "/site":
			if r.UserAgent() != "Custom/1" {
				t.Error(r.UserAgent())
			}
			fmt.Fprint(w, `<link rel="icon" href="images/favicon.svg">`)
		case "/large-head":
			fmt.Fprint(w, `<head><link rel="icon" href="/head.svg"></head>`+strings.Repeat("x", 256<<10))
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", (256<<10)+1))
		case "/slow":
			slow.Store(true)
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	a := testApp(t)
	icon, err := a.poller.fetchSiteIcon(t.Context(), srv.URL+"/redirect", "Custom/1")
	if err != nil || icon != srv.URL+"/images/favicon.svg" {
		t.Fatal(icon, err)
	}
	if icon, err := a.poller.fetchSiteIcon(t.Context(), srv.URL+"/large-head", "Custom/1"); err != nil || icon != srv.URL+"/head.svg" {
		t.Fatal(icon, err)
	}
	for _, path := range []string{"/large", "/error"} {
		if _, err = a.poller.fetchSiteIcon(t.Context(), srv.URL+path, "Custom/1"); err == nil {
			t.Fatal(path)
		}
	}
	a.poller.config.Poll.Timeout = "20ms"
	if _, err = a.poller.fetchSiteIcon(t.Context(), srv.URL+"/slow", "Custom/1"); err == nil || !slow.Load() {
		t.Fatal(err)
	}
	if _, err = a.poller.fetchSiteIcon(t.Context(), "https://example.org/%zz", "Custom/1"); err == nil {
		t.Fatal("bad URL accepted")
	}
	if _, err = a.poller.fetchSiteIcon(t.Context(), "file:///tmp/icon", "Custom/1"); err == nil {
		t.Fatal("non-HTTP URL accepted")
	}
	config := testConfig(t)
	config.Poll.AllowPrivate = false
	guarded := NewPoller(a.store, config, a.metrics)
	defer guarded.client.CloseIdleConnections()
	if _, err = guarded.fetchSiteIcon(t.Context(), srv.URL+"/site", "Custom/1"); err == nil {
		t.Fatal("private address allowed without config authorization")
	}
	// Optional discovery never waits on another occupied host while holding a feed host.
	u, _ := url.Parse(srv.URL)
	host := hostGroup(u)
	held := 0
	for a.poller.reserveHost(host) {
		held++
	}
	defer func() {
		for range held {
			a.poller.releaseHost(host)
		}
	}()
	ctx := context.WithValue(t.Context(), hostLeaseKey{}, &hostLease{keys: map[string]bool{"other": true}})
	if _, err = a.poller.fetchSiteIcon(ctx, srv.URL+"/site", "Custom/1"); err == nil {
		t.Fatal("busy host was not deferred")
	}
}

func TestRSSIconLookupFailureDoesNotFailSync(t *testing.T) {
	var address string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			fmt.Fprintf(w, `<rss version="2.0"><channel><title>Feed</title><link>%s/site</link><description>x</description><item><guid>one</guid></item></channel></rss>`, address)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	address = srv.URL
	a := testApp(t)
	a.poller.findSiteIcon = a.poller.fetchSiteIcon
	id := addFeed(t, a, address+"/feed")
	a.poller.fetch(t.Context(), id)
	feeds, err := a.store.Feeds()
	if err != nil || feeds[0].Count != 1 || feeds[0].Error != "" || feeds[0].Icon != address+"/favicon.ico" {
		t.Fatal(feeds, err)
	}
	now := time.Now().Unix()
	var checked int64
	if err = a.store.DB.QueryRow("SELECT icon_checked FROM feeds WHERE id=?", id).Scan(&checked); err != nil || checked > now-80000 || checked < now-84000 {
		t.Fatal(checked, err)
	}
}

func TestRSSIconDiscoveryCancellationDoesNotCountAsFeedFailure(t *testing.T) {
	started := make(chan struct{})
	var address string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			fmt.Fprintf(w, `<rss version="2.0"><channel><title>Feed</title><link>%s/site</link><description>x</description><item><guid>one</guid></item></channel></rss>`, address)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	address = srv.URL
	a := testApp(t)
	a.poller.findSiteIcon = a.poller.fetchSiteIcon
	id := addFeed(t, a, address+"/feed")
	done := make(chan struct{})
	go func() { defer close(done); a.poller.fetch(t.Context(), id) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("icon request did not start")
	}
	a.poller.Cancel(id)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled lookup did not stop")
	}
	if a.metrics.GetOrCreateCounter("roi_fetch_errors_total").Get() != 0 {
		t.Fatal("cancellation counted as a feed failure")
	}
	feeds, err := a.store.Feeds()
	if err != nil || feeds[0].Error != "" || feeds[0].Failures != 0 || feeds[0].Count != 0 {
		t.Fatal(feeds, err)
	}
}
