package roi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestStreamFiltersOrderingAndState(t *testing.T) {
	a := testApp(t)
	one := addFeed(t, a, "https://example.org/one")
	two := addFeed(t, a, "https://example.org/two")
	save(t, a, one, &gofeed.Item{GUID: "1", Title: "one"})
	save(t, a, two, &gofeed.Item{GUID: "2", Title: "two"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1,updated=20 WHERE feed_id=?", one)
	mustExec(t, a, "UPDATE entries SET updated=30 WHERE feed_id=?", two)
	mustExec(t, a, "INSERT INTO folders VALUES('Dev')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Dev')", one)
	token := approvedToken(t, a)
	for _, tc := range []struct {
		name string
		v    url.Values
		want []string
	}{
		{"all", nil, []string{"two", "one"}}, {"oldest", url.Values{"r": {"o"}}, []string{"one", "two"}},
		{"read", url.Values{"s": {statePrefix + "read"}}, []string{"one"}}, {"unread", url.Values{"s": {statePrefix + "unread"}}, []string{"two"}},
		{"starred", url.Values{"s": {statePrefix + "starred"}}, []string{"one"}}, {"kept unread", url.Values{"s": {"user/1/state/com.google/kept-unread"}}, []string{"two"}},
		{"folder", url.Values{"s": {"user/-/label/Dev"}}, []string{"one"}}, {"feed", url.Values{"s": {fmt.Sprintf("feed/%d", two)}}, []string{"two"}},
		{"union", url.Values{"s": {fmt.Sprintf("feed/%d", one), fmt.Sprintf("feed/%d", two)}}, []string{"two", "one"}},
		{"exclude", url.Values{"xt": {statePrefix + "read"}}, []string{"two"}}, {"include", url.Values{"it": {statePrefix + "starred"}}, []string{"one"}},
		{"time", url.Values{"ot": {"25"}, "nt": {"35"}}, []string{"two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiReq(a, token, "GET", "/reader/api/0/stream/contents/reading-list", tc.v) // The path identifies a stream, so use item IDs endpoint to apply s filters too.
			if len(tc.v["s"]) > 0 {
				w = apiReq(a, token, "GET", "/reader/api/0/stream/contents/"+tc.v.Get("s"), tc.v)
			}
			if tc.name == "union" {
				w = apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", tc.v)
				var got struct {
					Refs []any `json:"itemRefs"`
				}
				if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || len(got.Refs) != 2 {
					t.Fatal(w.Body.String(), e)
				}
				return
			}
			var got struct {
				Items []struct{ Title string } `json:"items"`
			}
			if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || w.Code != 200 {
				t.Fatal(w.Code, w.Body.String(), e)
			}
			if len(got.Items) != len(tc.want) {
				t.Fatal(w.Body.String())
			}
			for i, want := range tc.want {
				if got.Items[i].Title != want {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
	for _, v := range []url.Values{{"s": {"unknown"}}, {"s": {"feed/no"}}, {"xt": {"unknown"}}, {"it": {"unknown"}}, {"ot": {"invalid!"}}, {"nt": {"-1"}}, {"n": {"0"}}, {"n": {"invalid!"}}, {"c": {"tampered"}}} {
		w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", v)
		if w.Code != 400 {
			t.Fatal(v, w.Code, w.Body.String())
		}
	}
	for _, tag := range []string{statePrefix + "read", statePrefix + "starred", statePrefix + "kept-unread"} {
		for _, op := range []string{"a", "r"} {
			w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", url.Values{"i": {"1"}, op: {tag}})
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		}
	}
	for _, v := range []url.Values{{"i": {"invalid!"}}, {"i": {"1"}, "a": {"unknown"}}, {"i": {"1"}, "r": {"unknown"}}} {
		if w := apiReq(a, token, "POST", "/reader/api/0/edit-tag", v); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for _, v := range []url.Values{{"ts": {"invalid!"}}, {"ts": {"-1"}}, {"s": {"unknown"}}} {
		if w := apiReq(a, token, "POST", "/reader/api/0/mark-all-as-read", v); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}

func TestSubscriptionAndFolderOperations(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/feed")
	token := approvedToken(t, a)
	s := fmt.Sprintf("feed/%d", id)
	post := func(path string, v url.Values, want int) {
		t.Helper()
		w := apiReq(a, token, "POST", "/reader/api/0/"+path, v)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	post("subscription/edit", url.Values{"s": {s}, "ac": {"edit"}, "t": {"Renamed"}, "a": {"user/-/label/Old"}}, 200)
	post("rename-tag", url.Values{"s": {"user/-/label/Old"}, "dest": {"user/-/label/New"}}, 200)
	fs, e := a.store.Feeds()
	if e != nil || fs[0].Title != "Renamed" || len(fs[0].Folders) != 1 || fs[0].Folders[0] != "New" {
		t.Fatal(fs, e)
	}
	post("rename-tag", url.Values{"s": {"user/-/label/New"}, "dest": {"user/-/label/New"}}, 200)
	post("subscription/edit", url.Values{"s": {s}, "ac": {"edit"}, "r": {"user/-/label/New"}}, 200)
	post("subscription/edit", url.Values{"s": {s}, "ac": {"edit"}, "a": {"user/-/label/New"}}, 200)
	post("disable-tag", url.Values{"s": {"user/-/label/New"}}, 200)
	fs, e = a.store.Feeds()
	if e != nil || len(fs[0].Folders) != 0 {
		t.Fatal(fs, e)
	}
	for _, tc := range []struct {
		p string
		v url.Values
	}{
		{"rename-tag", url.Values{"s": {"invalid!"}}}, {"rename-tag", url.Values{"s": {"user/-/label/Old"}, "dest": {"invalid!"}}}, {"disable-tag", url.Values{"s": {"invalid!"}}},
		{"subscription/edit", url.Values{"s": {"invalid!"}, "ac": {"edit"}}}, {"subscription/edit", url.Values{"s": {"feed/999"}, "ac": {"edit"}}}, {"subscription/edit", url.Values{"s": {s}, "ac": {"invalid!"}}},
		{"subscription/edit", url.Values{"s": {s}, "ac": {"edit"}, "a": {"invalid!"}}}, {"subscription/edit", url.Values{"s": {s}, "ac": {"subscribe"}}}, {"subscription/quickadd", url.Values{"quickadd": {"file:///etc/passwd"}}},
	} {
		post(tc.p, tc.v, 400)
	}
	// A later invalid folder must roll back an earlier title change.
	post("subscription/edit", url.Values{"s": {s}, "ac": {"edit"}, "t": {"Must roll back"}, "a": {"invalid!"}}, 400)
	fs, e = a.store.Feeds()
	if e != nil || fs[0].Title != "Renamed" {
		t.Fatal("partial edit committed", fs, e)
	}
	post("subscription/edit", url.Values{"s": {s}, "ac": {"unsubscribe"}}, 200)
	post("subscription/edit", url.Values{"s": {s}, "ac": {"unsubscribe"}}, 200)
	post("subscription/quickadd", url.Values{"quickadd": {"https://example.org/feed"}}, 200)
	fs, e = a.store.Feeds()
	if e != nil || len(fs) != 1 || !fs[0].Active {
		t.Fatal(fs, e)
	}
}

func TestAPIValidationAndAuthentication(t *testing.T) {
	a := testApp(t)
	token := approvedToken(t, a)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/accounts/ClientLogin", 405}, {"DELETE", "/reader/api/0/user-info", 405}, {"GET", "/reader/api/0/edit-tag", 405}, {"GET", "/reader/api/0/missing", 404},
	} {
		if w := apiReq(a, token, tc.method, tc.path, nil); w.Code != tc.want {
			t.Fatal(tc, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/wrong-prefix", nil)
	w := httptest.NewRecorder()
	a.APIHandler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {"reader"}, "Passwd": {"wrong"}}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/accounts/ClientLogin", "/reader/api/0/edit-tag"} {
		r := httptest.NewRequest("POST", "/greader"+path, strings.NewReader("i=%zz"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Authorization", "GoogleLogin auth="+token)
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	ids := make([]string, 10001)
	for i := range ids {
		ids[i] = "1"
	}
	for _, route := range []string{"stream/items/contents", "edit-tag"} {
		if w := apiReq(a, token, "POST", "/reader/api/0/"+route, url.Values{"i": ids}); w.Code != 400 {
			t.Fatal(route, w.Code)
		}
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"invalid!"}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"999"}}); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w.Body.String())
	}
	for range 10 {
		a.allowLogin(httptest.NewRequest("POST", "/greader/accounts/ClientLogin", nil))
	}
	if w := apiReq(a, "", "POST", "/accounts/ClientLogin", nil); w.Code != 429 {
		t.Fatal(w.Code)
	}
}

func TestSignedCursorRejectionAndQueryBinding(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "1"}, &gofeed.Item{GUID: "2"})
	token := approvedToken(t, a)
	w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", url.Values{"n": {"1"}})
	var first struct {
		C string `json:"continuation"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &first); e != nil || first.C == "" {
		t.Fatal(w.Body.String(), e)
	}
	for _, v := range []url.Values{{"c": {first.C}, "r": {"o"}}, {"c": {first.C}, "xt": {statePrefix + "read"}}, {"c": {first.C + "x"}}} {
		if w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", v); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for _, raw := range []string{"!", base64.RawURLEncoding.EncodeToString([]byte("not json"))} {
		signed := raw + "." + sign(a.apiKey(), "cursor:"+raw)
		if _, e := a.decodeCursor(signed); e == nil {
			t.Fatal("invalid signed payload accepted")
		}
	}
}

func TestAPIAndOpsErrorsDoNotExposeDatabase(t *testing.T) {
	a := testApp(t)
	token := approvedToken(t, a)
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"subscription/list", "tag/list", "unread-count", "stream/items/ids", "stream/items/contents"} {
		w := apiReq(a, token, "POST", "/reader/api/0/"+path, url.Values{"i": {"1"}})
		if w.Code != 500 || strings.Contains(w.Body.String(), "sql") {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	a.OpsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	a.OpsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	h := a.wrap("test", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test recovery") }))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "test recovery") {
		t.Fatal(w.Body.String())
	}
}
