package roi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConsecutiveSyncErrorsShownPersistedAndReset(t *testing.T) {
	var status atomic.Int32
	status.Store(500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := int(status.Load())
		w.WriteHeader(code)
		if code == 200 {
			_, _ = io.WriteString(w, `<rss version="2.0"><channel><title>Counter feed</title><link>https://example.org</link><description>Test</description><item><guid>one</guid><title>One</title></item></channel></rss>`)
		}
	}))
	defer server.Close()
	c := testConfig(t)
	a, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { a.Close() }()
	id := addFeed(t, a, server.URL)
	check := func(want int64) {
		t.Helper()
		feeds, e := a.store.Feeds()
		if e != nil || len(feeds) != 1 || feeds[0].Failures != want {
			t.Fatal(feeds, e)
		}
		cookie, _ := adminSession(t, a)
		page := adminRequest(a, cookie, "GET", "/", nil)
		if page.Code != 200 || !strings.Contains(page.Body.String(), "Consecutive errors") || !strings.Contains(page.Body.String(), fmt.Sprintf(`<td class="failures">%d</td>`, want)) {
			t.Fatal(page.Code, page.Body.String())
		}
	}
	a.poller.fetch(context.Background(), id)
	check(1)
	status.Store(503)
	a.poller.fetch(context.Background(), id)
	check(2)
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	a, e = New(c)
	if e != nil {
		t.Fatal(e)
	}
	check(2)
	status.Store(200)
	a.poller.fetch(context.Background(), id)
	check(0)
	status.Store(500)
	a.poller.fetch(context.Background(), id)
	check(1)
	status.Store(304)
	a.poller.fetch(context.Background(), id)
	check(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.poller.fetch(ctx, id)
	check(0)
}
