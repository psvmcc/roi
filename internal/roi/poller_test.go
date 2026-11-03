package roi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollConditionalAndErrors(t *testing.T) {
	a := testApp(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `<rss version="2.0"><channel><title>Test</title><link>https://example.org</link><description>test</description><item><guid>abc</guid><title>News</title><description>Hello</description></item></channel></rss>`)
	}))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	a.poller.fetch(context.Background(), id)
	a.poller.fetch(context.Background(), id)
	var count, status int
	if e := a.store.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count); e != nil {
		t.Fatal(e)
	}
	if e := a.store.DB.QueryRow("SELECT status FROM fetches ORDER BY id DESC LIMIT 1").Scan(&status); e != nil {
		t.Fatal(e)
	}
	if count != 1 || status != 304 || calls.Load() != 2 {
		t.Fatalf("count=%d status=%d", count, status)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(503) }))
	defer bad.Close()
	id = addFeed(t, a, bad.URL)
	a.poller.fetch(context.Background(), id)
	var next int64
	var message string
	if e := a.store.DB.QueryRow("SELECT next_fetch,error FROM feeds WHERE id=?", id).Scan(&next, &message); e != nil {
		t.Fatal(e)
	}
	if next < time.Now().Add(115*time.Second).Unix() || message == "" {
		t.Fatal(next, message)
	}
}

func TestSchedulerAndGracefulShutdown(t *testing.T) {
	a := testApp(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title><id>test</id><updated>2026-01-01T00:00:00Z</updated><entry><id>atom-1</id><title>Entry</title><updated>2026-01-01T00:00:00Z</updated><content>Body</content></entry></feed>`)
	}))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.poller.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("poller did not stop")
		}
	}()
	deadline := time.Now().Add(4 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatal("scheduler did not run repeatedly")
	}
	mustExec(t, a, "UPDATE feeds SET paused=1 WHERE id=?", id)
	time.Sleep(100 * time.Millisecond)
	before := calls.Load()
	time.Sleep(1200 * time.Millisecond)
	if calls.Load() != before {
		t.Fatal("paused feed was polled")
	}
	fs, e := a.store.Feeds()
	if e != nil || !fs[0].Active || !fs[0].Paused {
		t.Fatal(fs, e)
	}
	var out bytes.Buffer
	if e = a.store.Export(&out); e != nil || !strings.Contains(out.String(), server.URL) {
		t.Fatal("paused subscription missing from OPML", e)
	}
}

func TestPrivateNetworksAndOversizedFeeds(t *testing.T) {
	a := testApp(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 2048)) }))
	defer server.Close()
	id := addFeed(t, a, server.URL)
	a.config.Poll.AllowPrivate = false
	p := NewPoller(a.store, a.config, a.metrics)
	p.fetch(context.Background(), id)
	var message string
	if e := a.store.DB.QueryRow("SELECT error FROM feeds WHERE id=?", id).Scan(&message); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(message, "blocked") {
		t.Fatal("private network not blocked", message)
	}
	a.poller.config.Poll.MaxBytes = 1024
	a.poller.fetch(context.Background(), id)
	if e := a.store.DB.QueryRow("SELECT error FROM feeds WHERE id=?", id).Scan(&message); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(message, "max_bytes") {
		t.Fatal("oversized feed accepted", message)
	}
}
