package roi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSessionIPHistoryIndividualRevocationAndRedaction(t *testing.T) {
	a := testApp(t)
	cookie, csrf := adminSession(t, a)
	first, second := approvedToken(t, a), approvedToken(t, a)
	for _, ip := range []string{"198.51.100.10:1234", "198.51.100.11:2345"} {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info?Passwd=secret-query", nil)
		r.RemoteAddr = ip
		r.Header.Set("Authorization", "GoogleLogin auth="+first)
		r.Header.Set("User-Agent", "Reeder/test")
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	list, err := a.sessionList(sessionID(cookie.Value))
	if err != nil || len(list) != 3 {
		t.Fatal(len(list), err)
	}
	var found bool
	for _, s := range list {
		if s.ID == sessionID(first) {
			found = true
			if s.FirstIP != "198.51.100.10" || s.LastIP != "198.51.100.11" || s.Requests != 2 || s.Client != "Reeder/test" || s.State != "Active" {
				t.Fatal(s)
			}
		}
	}
	if !found {
		t.Fatal("missing API session")
	}
	history, err := a.securityHistory()
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
	b, _ := json.Marshal(history)
	if strings.Contains(string(b), first) || strings.Contains(string(b), "secret-query") {
		t.Fatal("audit contains secret")
	}
	w := asyncAdmin(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {csrf}, "session": {sessionID(first)}})
	if w.Code != 200 || a.validToken(first) || !a.validToken(second) {
		t.Fatal("individual revocation failed", w.Code, w.Body.String())
	}
	history, err = a.securityHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 || history[0].Path != "/revoke-session" || history[0].Realm != "admin" {
		t.Fatal(history)
	}
	page := adminRequest(a, cookie, "GET", "/security?session="+sessionID(first), nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Session revoked") || strings.Contains(page.Body.String(), first) {
		t.Fatal("session UI missing metadata or leaked token")
	}
	if w := asyncAdmin(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {csrf}, "session": {"missing"}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := asyncAdmin(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {"wrong"}, "session": {sessionID(second)}}); w.Code != 403 || !a.validToken(second) {
		t.Fatal("missing CSRF protection")
	}
	// Fragment polling and static resources do not flood persisted history.
	before, _ := a.securityHistory()
	_ = adminRequest(a, cookie, "GET", "/?fragment=1", nil)
	_ = adminRequest(a, nil, "GET", "/assets/admin.js", nil)
	after, _ := a.securityHistory()
	if len(after) != len(before) {
		t.Fatal("background request logged")
	}
	w = asyncAdmin(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {csrf}, "session": {sessionID(cookie.Value)}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/login") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = asyncAdmin(a, cookie, "GET", "/", nil); w.Code != 401 {
		t.Fatal("revoked browser remains authorized", w.Code)
	}
}
func TestSessionRestartExpiryAndStorageFailures(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	token := approvedToken(t, a)
	cookie, _ := adminSession(t, a)
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.validToken(token) || !a.sessionActive(sessionID(cookie.Value)) {
		t.Fatal("restart session policy failed")
	}
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().Add(-time.Second).Unix(), sessionID(token))
	if a.validToken(token) {
		t.Fatal("expired session accepted")
	}
	list, err := a.sessionList("")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == sessionID(token) {
			t.Fatal(s)
		}
	}
	a.Close()
	if approvedToken(t, a) != "" {
		t.Fatal("token issued without persistent session")
	}
	if _, err = a.sessionList(""); err == nil {
		t.Fatal("closed database ignored")
	}
	if _, err = a.securityHistory(); err == nil {
		t.Fatal("closed history ignored")
	}
	if err = a.revokeSession(sessionID(token)); err == nil {
		t.Fatal("closed revocation ignored")
	}
	if shortSession("") != "—" || shortSession("short") != "short…" {
		t.Fatal("short session display")
	}
}

func TestSessionHistoryRetentionAndBound(t *testing.T) {
	a := testApp(t)
	// Import an oversized event history in one transaction to exercise the cap.
	mustExec(t, a, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10005) INSERT INTO auth_events(created,realm,session_id,method,path,ip,client,status) SELECT ?, 'api', '', 'GET', '/reader/api/0/user-info', '127.0.0.1', '', 401 FROM n`, time.Now().Unix())
	var count int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM auth_events").Scan(&count); err != nil || count != 10000 {
		t.Fatal(count, err)
	}
	token := approvedToken(t, a)
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().AddDate(0, 0, -31).Unix(), sessionID(token))
	mustExec(t, a, "UPDATE auth_events SET created=? WHERE id=(SELECT min(id) FROM auth_events)", time.Now().AddDate(0, 0, -31).Unix())
	a.poller.maintain(context.Background())
	if err := a.store.DB.QueryRow("SELECT count(*) FROM auth_events").Scan(&count); err != nil || count != 9999 {
		t.Fatal(count, err)
	}
	if err := a.store.DB.QueryRow("SELECT count(*) FROM auth_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	a.Close()
	a.poller.maintain(context.Background())
}

func TestSessionFailureResponsesAndAtomicRotation(t *testing.T) {
	a := testApp(t)
	cookie, csrf := adminSession(t, a)
	token := approvedToken(t, a)
	key, _ := a.store.SigningKey()
	mustExec(t, a, `CREATE TRIGGER fail_revoke BEFORE UPDATE OF revoked ON auth_sessions BEGIN SELECT RAISE(ABORT,'storage unavailable'); END`)
	if err := a.RotateAPIKey(); err == nil {
		t.Fatal("rotation ignored failure")
	}
	after, _ := a.store.SigningKey()
	if key != after || !a.validToken(token) {
		t.Fatal("failed rotation was not atomic")
	}
	w := asyncAdmin(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {csrf}, "session": {sessionID(token)}})
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	mustExec(t, a, "DROP TRIGGER fail_revoke")
	mustExec(t, a, `CREATE TRIGGER fail_login BEFORE INSERT ON auth_sessions BEGIN SELECT RAISE(ABORT,'storage unavailable'); END`)
	w = adminRequest(a, nil, "POST", "/login", url.Values{"username": {"admin"}, "password": {"test-password-123"}})
	if w.Code != 500 {
		t.Fatal("admin failed to reject unavailable session storage", w.Code)
	}
	w = apiReq(a, "", "POST", "/accounts/ClientLogin", deviceLoginForm(t, a))
	if w.Code != 503 {
		t.Fatal("API failed to reject unavailable session storage", w.Code)
	}
	mustExec(t, a, "DROP TRIGGER fail_login")
	mustExec(t, a, "UPDATE auth_sessions SET created='corrupted' WHERE id=?", sessionID(token))
	if _, err := a.sessionList(""); err == nil {
		t.Fatal("corrupted session accepted")
	}
	w = adminRequest(a, cookie, "GET", "/devices", nil)
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	mustExec(t, a, "DELETE FROM auth_sessions WHERE id=?", sessionID(token))
	mustExec(t, a, "UPDATE auth_events SET created='corrupted'")
	if _, err := a.securityHistory(); err == nil {
		t.Fatal("corrupted event accepted")
	}
	w = adminRequest(a, cookie, "GET", "/security", nil)
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	mustExec(t, a, "DELETE FROM auth_events")
	// The non-JavaScript fallback also signs out a revoked current session.
	w = adminRequest(a, cookie, "POST", "/revoke-session", url.Values{"csrf": {csrf}, "session": {sessionID(cookie.Value)}})
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal(w.Code)
	}
	a.Close()
	a.touchSession(httptest.NewRequest("GET", "/", nil), sessionID(token))
}

func TestSessionStartupRejectsInvalidTable(t *testing.T) {
	c := testConfig(t)
	s, err := OpenStore(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	rollbackReviewMigration(t, s.DB)
	_, err = s.DB.Exec("DROP TABLE auth_sessions; CREATE VIEW auth_sessions AS SELECT 1 AS id; ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=2")
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	// A conflicting view prevents a valid migration; avoid partially advancing the version.
	// SQLite IF NOT EXISTS accepts an existing view, so startup must still reject it.
	if a, err := New(c); err == nil {
		a.Close()
		t.Fatal("invalid sessions schema accepted")
	}
}

func TestSessionPanelsOnlyShowActiveSessions(t *testing.T) {
	a := testApp(t)
	cookie, _ := adminSession(t, a)
	active, revoked, expired := approvedToken(t, a), approvedToken(t, a), approvedToken(t, a)
	if err := a.revokeSession(sessionID(revoked)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().Add(-time.Second).Unix(), sessionID(expired))
	for _, suffix := range []string{"", "?fragment=1"} {
		w := adminRequest(a, cookie, "GET", "/devices"+suffix, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		body := w.Body.String()
		readerStart := strings.Index(body, `<section id="reader-sessions">`)
		if readerStart < 0 {
			t.Fatal("Reader sessions missing")
		}
		reader := strings.Split(body[readerStart:], "</section>")[0]
		browserPage := adminRequest(a, cookie, "GET", "/security"+suffix, nil).Body.String()
		browser := strings.Split(strings.Split(browserPage, `<section id="browser-sessions">`)[1], "</section>")[0]
		if !strings.Contains(reader, sessionID(active)) || strings.Contains(reader, sessionID(cookie.Value)) || !strings.Contains(reader, `action="/revoke-session"`) || !strings.Contains(reader, `action="/revoke-api-tokens"`) {
			t.Fatal("Reader session or revoke actions in wrong block")
		}
		if !strings.Contains(browser, sessionID(cookie.Value)) || strings.Contains(browser, sessionID(active)) {
			t.Fatal("browser session in wrong block")
		}
		for _, id := range []string{sessionID(revoked), sessionID(expired)} {
			if strings.Contains(reader, id) || strings.Contains(browser, id) {
				t.Fatal("inactive session displayed")
			}
		}
	}
	if err := a.revokeSession(sessionID(active)); err != nil {
		t.Fatal(err)
	}
	w := adminRequest(a, cookie, "GET", "/devices", nil)
	reader := strings.Split(strings.Split(w.Body.String(), `<section id="reader-sessions">`)[1], "</section>")[0]
	if !strings.Contains(reader, "No active sessions.") {
		t.Fatal("missing empty state")
	}
}
