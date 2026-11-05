package roi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the Reader protocol over HTTP, including a server restart, without
// depending on Reeder's UI or external feeds. Manual client compatibility still
// matters; this fixes the request/response behavior roi promises to clients.
func TestReederClassicSyncContractOverHTTP(t *testing.T) {
	var generation atomic.Int32
	generation.Store(2)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, `<rss version="2.0"><channel><title>Contract feed</title><link>https://example.org/</link><description>Test</description>`)
		for i := int32(1); i <= generation.Load(); i++ {
			fmt.Fprintf(w, `<item><guid>article-%d</guid><title>Article %d</title><pubDate>Wed, 14 Oct 2009 16:12:40 +0000</pubDate><description>Body %d</description></item>`, i, i, i)
		}
		_, _ = io.WriteString(w, `</channel></rss>`)
	}))
	defer source.Close()
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.APIHandler())
	defer func() { server.Close(); a.Close() }()
	client := &http.Client{Timeout: 3 * time.Second}
	auth := ""
	call := func(method, path string, values url.Values, want int) []byte {
		t.Helper()
		raw := server.URL + "/greader" + path
		var body io.Reader
		if method == "GET" {
			raw += "?" + values.Encode()
		} else {
			body = strings.NewReader(values.Encode())
		}
		request, err := http.NewRequest(method, raw, body)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("User-Agent", "ReederClassic/contract-test")
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth != "" {
			request.Header.Set("Authorization", "GoogleLogin auth="+auth)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want {
			t.Fatal(path, response.StatusCode, string(data), err)
		}
		return data
	}
	route := func(name string) string { return "/reader/api/0/" + name }
	login := call("POST", "/accounts/ClientLogin", deviceLoginForm(t, a), 200)
	for _, line := range strings.Split(string(login), "\n") {
		if strings.HasPrefix(line, "Auth=") {
			auth = strings.TrimPrefix(line, "Auth=")
		}
	}
	if auth == "" {
		t.Fatal("missing Reader token")
	}
	pending := call("GET", route("subscription/list"), nil, 200)
	if !strings.Contains(string(pending), `"subscriptions":[]`) {
		t.Fatal(string(pending))
	}
	call("POST", route("subscription/quickadd"), url.Values{"quickadd": {source.URL}}, 403)
	cookie, csrf := adminSession(t, a)
	if w := asyncAdmin(a, cookie, "POST", "/approve-session", url.Values{"csrf": {csrf}, "session": {sessionID(auth)}, "device_name": {"Reeder contract"}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	actionToken := strings.TrimSpace(string(call("GET", route("token"), nil, 200)))
	call("POST", route("subscription/quickadd"), url.Values{"quickadd": {source.URL}}, 200)
	var feed int64
	if err := a.store.DB.QueryRow("SELECT id FROM feeds WHERE url=?", source.URL).Scan(&feed); err != nil {
		t.Fatal(err)
	}
	a.poller.fetch(t.Context(), feed)
	type stream struct {
		Items []struct {
			ID         string   `json:"id"`
			Published  int64    `json:"published"`
			Categories []string `json:"categories"`
		} `json:"items"`
		Continuation string `json:"continuation"`
	}
	var first stream
	if err := json.Unmarshal(call("GET", route("stream/contents/reading-list"), url.Values{"n": {"1"}}, 200), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Continuation == "" || first.Items[0].Published != 1255536760 {
		t.Fatal(first)
	}
	var second stream
	if err := json.Unmarshal(call("GET", route("stream/contents/reading-list"), url.Values{"n": {"1"}, "c": {first.Continuation}}, 200), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Fatal(second)
	}
	item := first.Items[0].ID
	call("POST", route("edit-tag"), url.Values{"T": {actionToken}, "i": {item}, "a": {statePrefix + "read", statePrefix + "starred"}}, 200)
	content := call("POST", route("stream/items/contents"), url.Values{"i": {item}}, 200)
	if !strings.Contains(string(content), "state/com.google/starred") || !strings.Contains(string(content), "state/com.google/read") {
		t.Fatal(string(content))
	}
	feedID := "feed/" + strconv.FormatInt(feed, 10)
	call("POST", route("subscription/edit"), url.Values{"s": {feedID}, "ac": {"edit"}, "t": {"Renamed"}, "a": {"user/-/label/Contract"}}, 200)
	if data := call("GET", route("subscription/list"), nil, 200); !strings.Contains(string(data), "Renamed") || !strings.Contains(string(data), "Contract") {
		t.Fatal(string(data))
	}
	generation.Store(3)
	a.poller.fetch(t.Context(), feed)
	a.poller.fetch(t.Context(), feed)
	var count int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil || count != 3 {
		t.Fatal("incremental sync duplicated entries", count, err)
	}
	server.Close()
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(a.APIHandler())
	// The same approved bearer token, action token and article ID remain valid.
	content = call("POST", route("stream/items/contents"), url.Values{"i": {item}}, 200)
	if !strings.Contains(string(content), "state/com.google/starred") {
		t.Fatal("star lost after restart", string(content))
	}
	call("POST", route("mark-all-as-read"), url.Values{"s": {feedID}, "T": {actionToken}}, 200)
	if err := a.store.DB.QueryRow("SELECT count(*) FROM entries WHERE is_read=0").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	call("POST", route("subscription/edit"), url.Values{"s": {feedID}, "ac": {"unsubscribe"}}, 200)
	if data := call("GET", route("subscription/list"), nil, 200); !strings.Contains(string(data), `"subscriptions":[]`) {
		t.Fatal(string(data))
	}
	if err := a.store.DB.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil || count != 3 {
		t.Fatal("unsubscribe deleted history", count, err)
	}
	var active bool
	if err := a.store.DB.QueryRow("SELECT active FROM feeds WHERE id=?", feed).Scan(&active); err != nil || active {
		t.Fatal(active, err)
	}
}
