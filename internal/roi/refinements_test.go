package roi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestApprovedReederSubscriptionWithClientActionToken(t *testing.T) {
	a := testApp(t)
	approved, pending := approvedToken(t, a), a.issueToken()
	for _, route := range []string{"subscription/quickadd", "subscription/edit"} {
		raw := "https://example.org/" + strings.ReplaceAll(route, "/", "-")
		values := url.Values{"T": {"client-specific-token"}, "ac": {"subscribe"}, "s": {"feed/" + raw}, "quickadd": {raw}, "t": {"New feed"}}
		if w := apiReq(a, pending, "POST", "/reader/api/0/"+route, values); w.Code != 403 {
			t.Fatal(route, w.Code)
		}
		if w := apiReq(a, approved, "POST", "/reader/api/0/"+route, values); w.Code != 200 {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 2 {
		t.Fatal(feeds, err)
	}
}

func TestDebugDeviceLogsAndSecurityEventSnapshot(t *testing.T) {
	a := testApp(t)
	token := a.issueToken()
	cookie, csrf := adminSession(t, a)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	_ = apiReq(a, token, "GET", "/reader/api/0/user-info", nil)
	if logs.Len() != 0 {
		t.Fatal("request logs enabled by default", logs.String())
	}
	values := url.Values{"csrf": {csrf}, "session": {sessionID(token)}, "device_name": {"Reeder Mac"}}
	w := asyncAdmin(a, cookie, "POST", "/approve-session", values)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	events, err := a.securityHistory()
	if err != nil || events[0].Device != "Reeder Mac" || events[0].TargetSession != sessionID(token) || events[0].Session != sessionID(cookie.Value) {
		t.Fatal(events, err)
	}
	a.config.Debug = true
	_ = apiReq(a, token, "GET", "/reader/api/0/user-info", nil)
	if !strings.Contains(logs.String(), `"device":"Reeder Mac"`) || !strings.Contains(logs.String(), shortSession(sessionID(token))) {
		t.Fatal(logs.String())
	}
	pending := a.issueToken()
	_ = apiReq(a, pending, "GET", "/reader/api/0/user-info", nil)
	if !strings.Contains(logs.String(), `"device":"unapproved"`) {
		t.Fatal(logs.String())
	}
	if err := a.nameSession(sessionID(token), "Renamed", false); err != nil {
		t.Fatal(err)
	}
	events, err = a.securityHistory()
	if err != nil || events[0].Device != "Reeder Mac" {
		t.Fatal("event snapshot changed", events, err)
	}
}

func TestPausedFeedsFirstTitlesAndNoDisabledBadge(t *testing.T) {
	a := testApp(t)
	normal, failed, paused, disabled := addFeed(t, a, "https://example.org/normal"), addFeed(t, a, "https://example.org/failed"), addFeed(t, a, "https://example.org/paused"), addFeed(t, a, "https://example.org/disabled")
	mustExec(t, a, "UPDATE feeds SET title='A normal' WHERE id=?", normal)
	mustExec(t, a, "UPDATE feeds SET title='B failed',error='HTTP 503',failures=4 WHERE id=?", failed)
	mustExec(t, a, "UPDATE feeds SET title='Z paused',paused=1 WHERE id=?", paused)
	mustExec(t, a, "UPDATE feeds SET active=0 WHERE id=?", disabled)
	cookie, _ := adminSession(t, a)
	body := adminRequest(a, cookie, "GET", "/", nil).Body.String()
	if !strings.Contains(body, "<title>ροή</title>") || strings.Index(body, "Z paused") > strings.Index(body, "B failed") || strings.Index(body, "B failed") > strings.Index(body, "A normal") {
		t.Fatal("title or feed priority incorrect")
	}
	body = adminRequest(a, cookie, "GET", "/?disabled=1", nil).Body.String()
	if strings.Contains(body, `class="status-disabled"`) {
		t.Fatal("redundant disabled badge")
	}
}

func TestFeedFailureLogHasTitleAndCompleteURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	a := testApp(t)
	raw := server.URL + "/rss/private?key=visible"
	id := addFeed(t, a, raw)
	mustExec(t, a, "UPDATE feeds SET title='Example feed' WHERE id=?", id)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	a.poller.fetch(t.Context(), id)
	if !strings.Contains(logs.String(), raw) || !strings.Contains(logs.String(), "Example feed") || !strings.Contains(logs.String(), "HTTP 503") || strings.Contains(logs.String(), "redacted") {
		t.Fatal(logs.String())
	}
}

func TestBulkBrowserRevocationAndCSRF(t *testing.T) {
	a := testApp(t)
	cookie, csrf := adminSession(t, a)
	other, _ := adminSession(t, a)
	token := approvedToken(t, a)
	if w := asyncAdmin(a, cookie, "POST", "/revoke-browser-sessions", url.Values{"csrf": {"bad"}}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := asyncAdmin(a, cookie, "POST", "/revoke-browser-sessions", url.Values{"csrf": {csrf}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/login") || !a.validToken(token) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := asyncAdmin(a, other, "GET", "/", nil); w.Code != 401 {
		t.Fatal("other browser not revoked", w.Code)
	}
	if a.sessionActive(sessionID(cookie.Value)) {
		t.Fatal("current browser still active")
	}
}
