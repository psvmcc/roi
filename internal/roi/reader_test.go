package roi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestReaderAPI(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/feed")
	for i := range 5 {
		save(t, a, id, &gofeed.Item{GUID: fmt.Sprint(i), Title: fmt.Sprint(i)})
	}
	if w := apiReq(a, "", "GET", "/reader/api/0/user-info", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"test-password-123"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	token := strings.TrimSpace(strings.Split(w.Body.String(), "Auth=")[1])
	if err := a.nameSession(sessionID(token), "Test Reader", true); err != nil {
		t.Fatal(err)
	}
	if !a.validToken(token) || a.validToken(token+"a") {
		t.Fatal("token validation")
	}
	type stream struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Continuation string `json:"continuation"`
	}
	var first stream
	w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", url.Values{"n": {"2"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if e := json.Unmarshal(w.Body.Bytes(), &first); e != nil {
		t.Fatal(e)
	}
	if len(first.Items) != 2 || first.Continuation == "" {
		t.Fatal(w.Body.String())
	}
	save(t, a, id, &gofeed.Item{GUID: "new", Title: "new"})
	seen := map[string]bool{}
	for _, x := range first.Items {
		seen[x.ID] = true
	}
	c := first.Continuation
	for c != "" {
		w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", url.Values{"n": {"2"}, "c": {c}})
		var next stream
		if e := json.Unmarshal(w.Body.Bytes(), &next); e != nil {
			t.Fatal(w.Body.String())
		}
		for _, x := range next.Items {
			if seen[x.ID] {
				t.Fatal("duplicate during pagination")
			}
			seen[x.ID] = true
		}
		c = next.Continuation
	}
	if len(seen) != 5 {
		t.Fatalf("snapshot got %d items", len(seen))
	}
	csrf := sign(a.apiKey(), "csrf:"+token)
	item := first.Items[0].ID
	w = apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"T": {"invalid"}, "i": {item}, "a": {statePrefix + "read", statePrefix + "starred"}})
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"T": {csrf}, "i": {item}, "a": {statePrefix + "read", statePrefix + "starred"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {item}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "state/com.google/starred") {
		t.Fatal(w.Body.String())
	}
	w = apiReq(a, token, "POST", "/reader/api/0/subscription/quickadd", url.Values{"T": {csrf}, "quickadd": {"https://example.org/second"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = apiReq(a, token, "POST", "/reader/api/0/subscription/edit", url.Values{"T": {csrf}, "s": {fmt.Sprintf("feed/%d", id)}, "ac": {"edit"}, "a": {"user/-/label/Dev"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, p := range []string{"subscription/list", "tag/list", "unread-count", "user-info", "stream/items/ids"} {
		w = apiReq(a, token, "GET", "/reader/api/0/"+p, nil)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s %d %s", p, w.Code, w.Body.String())
		}
	}
	w = apiReq(a, token, "POST", "/reader/api/0/mark-all-as-read", url.Values{"T": {csrf}, "ts": {"1000000"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var n int
	if e := a.store.DB.QueryRow("SELECT COUNT(*) FROM entries WHERE is_read=0").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 5 {
		t.Fatal("mark-all read newer entries", n)
	}
}

func TestMarkAllReadMicrosecondBoundary(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/feed")
	at := time.Now().Truncate(time.Second).Add(100 * time.Microsecond)
	for i := range 2 {
		_, e := a.poller.saveEntries(context.Background(), id, &gofeed.Feed{Items: []*gofeed.Item{{GUID: fmt.Sprint(i), Title: fmt.Sprint(i)}}}, at.Add(time.Duration(i)*time.Microsecond))
		if e != nil {
			t.Fatal(e)
		}
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "POST", "/reader/api/0/mark-all-as-read", url.Values{"T": {sign(a.apiKey(), "csrf:"+token)}, "ts": {strconv.FormatInt(at.UnixMicro(), 10)}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var n int
	if e := a.store.DB.QueryRow("SELECT COUNT(*) FROM entries WHERE is_read=1").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 1 {
		t.Fatalf("read %d entries across time boundary", n)
	}
}

func TestReaderPagingAndLegacyParameters(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	for i := range 3 {
		save(t, a, id, &gofeed.Item{GUID: fmt.Sprint(i), Title: fmt.Sprint(i)})
	}
	token := approvedToken(t, a)
	values := url.Values{"s": {statePrefix + "reading-list"}, "xt": {statePrefix + "read"}, "n": {"1"}, "ck": {"1"}, "output": {"json"}}
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", values)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var result struct {
			Refs []struct {
				ID string `json:"id"`
			} `json:"itemRefs"`
			Continuation *string `json:"continuation"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
			t.Fatal(e)
		}
		if len(result.Refs) != 1 || seen[result.Refs[0].ID] {
			t.Fatal("invalid page", w.Body.String())
		}
		ref := result.Refs[0].ID
		seen[ref] = true
		contents := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i[]": {ref}})
		if contents.Code != 200 || !strings.Contains(contents.Body.String(), "tag:google.com,2005:reader/item/") {
			t.Fatal(contents.Body.String())
		}
		if page == 2 {
			if result.Continuation != nil {
				t.Fatal("terminal page must omit continuation", w.Body.String())
			}
		} else {
			if result.Continuation == nil || *result.Continuation == "" {
				t.Fatal("missing continuation")
			}
			values.Set("c", *result.Continuation)
			values.Set("ck", fmt.Sprint(page+2))
			values.Set("client", "reeder")
		}
	}
	for _, actionToken := range []string{"x", "", a.actionToken(token)} {
		w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"T": {actionToken}, "i[]": {"1"}, "a[]": {statePrefix + "read"}})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	w := apiReq(a, token, "GET", "/reader/api/0/token", nil)
	if w.Code != 200 || len(strings.TrimSpace(w.Body.String())) != 57 {
		t.Fatal("invalid action token")
	}
	for _, p := range []string{"preference/list", "preference/stream/list"} {
		w := apiReq(a, token, "GET", "/reader/api/0/"+p, nil)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatal(p, w.Body.String())
		}
	}
	w = apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", url.Values{"s": {statePrefix + "unread"}, "n": {"10000"}})
	var unread struct {
		Refs []any   `json:"itemRefs"`
		C    *string `json:"continuation"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &unread); e != nil {
		t.Fatal(e)
	}
	if w.Code != 200 || len(unread.Refs) != 2 || unread.C != nil {
		t.Fatal(w.Body.String())
	}
	w = apiReq(a, "", "POST", "/reader/api/0/edit-tag", url.Values{"T": {"x"}, "i": {"1"}, "a": {statePrefix + "read"}})
	if w.Code != 401 {
		t.Fatal("legacy token bypassed authentication")
	}
}

func TestReaderItemIDEncodings(t *testing.T) {
	for raw, want := range map[string]int64{"26": 26, "000000000000001a": 26, "0000000000000010": 16, "1a": 26, "0x1a": 26, "0X1A": 26, "tag:google.com,2005:reader/item/000000000000001a": 26} {
		got, e := parseEntryID(raw)
		if e != nil || got != want {
			t.Fatalf("%q => %d, %v; want %d", raw, got, e, want)
		}
	}
	for _, raw := range []string{"", "0", "not-an-id", "-1", "ffffffffffffffff"} {
		if _, e := parseEntryID(raw); e == nil {
			t.Fatalf("accepted invalid ID %q", raw)
		}
	}
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "one", Title: "One"})
	mustExec(t, a, "UPDATE entries SET id=26")
	token := approvedToken(t, a)
	for _, raw := range []string{"26", "000000000000001a", "1a", entryID(26)} {
		w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {raw}, "T": {"x"}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), entryID(26)) {
			t.Fatalf("contents for %q: %d %s", raw, w.Code, w.Body.String())
		}
	}
	w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"i": {"000000000000001a"}, "a": {statePrefix + "starred"}, "T": {"x"}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var starred bool
	if e := a.store.DB.QueryRow("SELECT starred FROM entries WHERE id=26").Scan(&starred); e != nil || !starred {
		t.Fatal("bare hexadecimal ID state change failed", e)
	}
}
