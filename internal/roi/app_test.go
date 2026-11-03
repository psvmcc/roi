package roi

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestPersistence(t *testing.T) {
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	id := addFeed(t, a, "https://example.org/feed")
	save(t, a, id, &gofeed.Item{GUID: "persist", Title: "one"})
	token := approvedToken(t, a)
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	fs, e := b.store.Feeds()
	if e != nil || len(fs) != 1 || fs[0].Count != 1 {
		t.Fatal(fs, e)
	}
	if !b.validToken(token) {
		t.Fatal("API token lost on restart")
	}
}

func TestServerLifecycle(t *testing.T) {
	a := testApp(t)
	addresses := []string{}
	listeners := []net.Listener{}
	for range 3 {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		listeners = append(listeners, l)
		addresses = append(addresses, l.Addr().String())
	}
	for _, l := range listeners {
		if e := l.Close(); e != nil {
			t.Fatal(e)
		}
	}
	a.config.API.Listen = addresses[0]
	a.config.Admin.Listen = addresses[1]
	a.config.Ops.Listen = addresses[2]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("server shutdown timed out")
		}
	}()
	client := &http.Client{Timeout: time.Second}
	for i, path := range []string{"/greader/reader/api/0/user-info", "/login", "/health"} {
		deadline := time.Now().Add(3 * time.Second)
		var resp *http.Response
		var e error
		for time.Now().Before(deadline) {
			resp, e = client.Get("http://" + addresses[i] + path)
			if e == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		expected := 200
		if i == 0 {
			expected = 401
		}
		if resp.StatusCode != expected {
			t.Fatal(path, resp.StatusCode)
		}
	}
}
