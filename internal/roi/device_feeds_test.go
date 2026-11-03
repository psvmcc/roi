package roi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestDeviceFeedVisibilityIsolationPersistenceAndState(t *testing.T) {
	a := testApp(t)
	config := a.config
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "same", Title: "Article", Content: "Body"})
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1")
	var entry int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries WHERE feed_id=?", id).Scan(&entry); err != nil {
		t.Fatal(err)
	}
	hiddenToken, other := approvedToken(t, a), approvedToken(t, a)
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "session": {sessionID(hiddenToken)}, "hidden": {"1"}}
	w := asyncAdmin(a, cookie, "POST", "/device-feeds", values)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !a.validToken(hiddenToken) {
		t.Fatal("hide revoked the token")
	}
	if approved, err := a.sessionApproved(sessionID(hiddenToken)); err != nil || !approved {
		t.Fatal("hide removed approval", approved, err)
	}
	for _, route := range []string{"subscription/list", "tag/list", "unread-count", "stream/items/ids", "stream/contents/reading-list", "stream/contents/feed/" + fmt.Sprint(id)} {
		w = apiReq(a, hiddenToken, "GET", "/reader/api/0/"+route, nil)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "Article") || strings.Contains(w.Body.String(), "example.org") {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
	w = apiReq(a, hiddenToken, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {fmt.Sprint(entry)}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w.Body.String())
	}
	for _, route := range []string{"edit-tag", "subscription/quickadd", "mark-all-as-read"} {
		if w = apiReq(a, hiddenToken, "POST", "/reader/api/0/"+route, url.Values{"i": {fmt.Sprint(entry)}, "a": {statePrefix + "read"}, "quickadd": {"https://example.org/new"}}); w.Code != 403 {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
	if w = apiReq(a, other, "GET", "/reader/api/0/subscription/list", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "example.org") {
		t.Fatal("other device affected", w.Body.String())
	}
	if w = apiReq(a, hiddenToken, "GET", "/reader/api/0/user-info", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	body := adminRequest(a, cookie, "GET", "/", nil).Body.String()
	if !strings.Contains(body, `aria-label="Show feeds"`) || !strings.Contains(body, "Feeds hidden") {
		t.Fatal("hidden state missing in admin")
	}
	events, err := a.securityHistory()
	if err != nil || len(events) == 0 {
		t.Fatal(events, err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "Device feed visibility changed" && event.TargetSession == sessionID(hiddenToken) && event.Device != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("visibility event lacks device identity", events)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, hidden, err := a.readerAccess(sessionID(hiddenToken)); err != nil || !hidden {
		t.Fatal(hidden, err)
	}
	values.Set("hidden", "0")
	for range 2 {
		w = asyncAdmin(a, cookie, "POST", "/device-feeds", values)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w = apiReq(a, hiddenToken, "GET", "/reader/api/0/stream/contents/reading-list", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Body") || !strings.Contains(w.Body.String(), "state/com.google/starred") {
		t.Fatal(w.Code, w.Body.String())
	}
	var got int64
	var read, starred bool
	if err = a.store.DB.QueryRow("SELECT id,is_read,starred FROM entries WHERE feed_id=?", id).Scan(&got, &read, &starred); err != nil || got != entry || !read || !starred {
		t.Fatal(got, read, starred, err)
	}
}
func TestDeviceFeedVisibilityAuthorizationAndValidation(t *testing.T) {
	a := testApp(t)
	token := approvedToken(t, a)
	pending := a.issueToken()
	cookie, csrf := adminSession(t, a)
	base := url.Values{"csrf": {csrf}, "session": {sessionID(token)}, "hidden": {"1"}}
	if w := asyncAdmin(a, nil, "POST", "/device-feeds", base); w.Code != 401 {
		t.Fatal(w.Code)
	}
	bad := cloneValues(base)
	bad.Set("csrf", "wrong")
	if w := asyncAdmin(a, cookie, "POST", "/device-feeds", bad); w.Code != 403 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/device-feeds", strings.NewReader(base.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, target := range []string{sessionID(pending), sessionID(cookie.Value), "unknown"} {
		bad = cloneValues(base)
		bad.Set("session", target)
		if w := asyncAdmin(a, cookie, "POST", "/device-feeds", bad); w.Code != 400 {
			t.Fatal(target, w.Code)
		}
	}
	for _, hidden := range []string{"", "true", "2"} {
		bad = cloneValues(base)
		bad.Set("hidden", hidden)
		if w := asyncAdmin(a, cookie, "POST", "/device-feeds", bad); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/device-feeds", base); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, cookie, "GET", "/device-feeds", base); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if err := a.revokeSession(sessionID(token)); err != nil {
		t.Fatal(err)
	}
	if err := a.setDeviceFeedsHidden(sessionID(token), true); err == nil {
		t.Fatal("revoked session changed")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.setDeviceFeedsHidden(sessionID(pending), true); err == nil {
		t.Fatal("closed DB changed")
	}
}
