package roi

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReaderSessionsNeverExpireAndSurviveMaintenance(t *testing.T) {
	c := testConfig(t)
	c.Security.TokenTTL = "1s" // Old configuration is accepted but ignored.
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	token := approvedToken(t, a)
	if !strings.HasPrefix(token, "v2.0.") {
		t.Fatal("new token has a deadline")
	}
	id := sessionID(token)
	var expires int64
	if err := a.store.DB.QueryRow("SELECT expires FROM auth_sessions WHERE id=?", id).Scan(&expires); err != nil || expires != 0 {
		t.Fatal(expires, err)
	}
	mustExec(t, a, "UPDATE auth_sessions SET created=?,last_seen=? WHERE id=?", time.Now().AddDate(-2, 0, 0).Unix(), time.Now().AddDate(-2, 0, 0).Unix(), id)
	a.poller.maintain(t.Context())
	if !a.validToken(token) {
		t.Fatal("maintenance removed permanent API session")
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	if !a.validToken(token) {
		t.Fatal("restart expired permanent API session")
	}
	if err := a.nameSession(id, "Renamed after restart", false); err != nil {
		t.Fatal(err)
	}
	if approved, err := a.sessionApproved(id); err != nil || !approved {
		t.Fatal(approved, err)
	}
	if err := a.revokeSession(id); err != nil {
		t.Fatal(err)
	}
	if a.validToken(token) {
		t.Fatal("permanent session cannot be revoked")
	}
	a.poller.maintain(t.Context())
	if a.sessionActive(id) {
		t.Fatal("revoked permanent session active")
	}
}

func TestActiveLegacyReaderSessionsUpgradeWithoutReauthentication(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	legacy := func(expiry time.Time) string {
		prefix := "v2." + strconv.FormatInt(expiry.Unix(), 10) + "." + randomToken()
		token := prefix + "." + sign(a.apiKey(), prefix)
		if err := a.createSession(token, "api", expiry, nil); err != nil {
			t.Fatal(err)
		}
		return token
	}
	active := legacy(time.Now().Add(time.Hour))
	revoked := legacy(time.Now().Add(time.Hour))
	expired := legacy(time.Now().Add(-time.Hour))
	if err := a.nameSession(sessionID(active), "Existing Reeder", true); err != nil {
		t.Fatal(err)
	}
	if err := a.revokeSession(sessionID(revoked)); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := adminSession(t, a)
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	if !a.validToken(active) || a.validToken(revoked) || a.validToken(expired) {
		t.Fatal("legacy upgrade changed access policy")
	}
	var expires int64
	if err := a.store.DB.QueryRow("SELECT expires FROM auth_sessions WHERE id=?", sessionID(active)).Scan(&expires); err != nil || expires != 0 {
		t.Fatal(expires, err)
	}
	if state, err := a.loadBrowserSession(cookie.Value); err != nil || state.CSRF != csrf || !state.Expires.After(time.Now()) {
		t.Fatal("upgrade changed browser lifetime", state, err)
	}
	// Model an upgraded token after its original signed deadline. SQLite's
	// no-expiry registration remains authoritative for the authenticated token.
	prefix := "v2." + fmt.Sprint(time.Now().Add(-time.Hour).Unix()) + "." + randomToken()
	oldStamp := prefix + "." + sign(a.apiKey(), prefix)
	if err := a.createSession(oldStamp, "api", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if !a.validToken(oldStamp) {
		t.Fatal("legacy timestamp still expires a registered permanent session")
	}
	for _, stamp := range []string{"-1", "not-a-number"} {
		prefix := "v2." + stamp + "." + randomToken()
		invalid := prefix + "." + sign(a.apiKey(), prefix)
		if err := a.createSession(invalid, "api", time.Time{}, nil); err != nil {
			t.Fatal(err)
		}
		if a.validToken(invalid) {
			t.Fatal("malformed signed token accepted")
		}
	}
}

func TestPermanentSessionUIAndBrowserExpiryIsolation(t *testing.T) {
	a := testApp(t)
	token := approvedToken(t, a)
	cookie, _ := adminSession(t, a)
	body := adminRequest(a, cookie, "GET", "/devices", nil).Body.String()
	body += adminRequest(a, cookie, "GET", "/security", nil).Body.String()
	if strings.Contains(body, "Type / status") || strings.Contains(body, "Never expires") || !strings.Contains(body, `data-session-realm="admin"`) || !strings.Contains(body, `data-session-realm="api"`) {
		t.Fatal("session UI missing expiry/realm information")
	}
	readerSection := strings.Split(strings.Split(body, `<section id="reader-sessions">`)[1], `</section>`)[0]
	if strings.Contains(readerSection, "expires") || !strings.Contains(readerSection, "<th>Created</th>") {
		t.Fatal("API expiry labels retained")
	}
	if !strings.Contains(body, "<th>Created / expires</th>") {
		t.Fatal("browser expiry missing")
	}
	if !strings.Contains(body, sessionID(token)) {
		t.Fatal("permanent session not listed")
	}
	mustExec(t, a, "UPDATE auth_sessions SET expires=0 WHERE id=?", sessionID(cookie.Value))
	if a.sessionActive(sessionID(cookie.Value)) {
		t.Fatal("browser received no-expiry exemption")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if got := string(adminTime(1255536760)); !strings.Contains(got, `data-unix="1255536760"`) || !strings.Contains(got, `datetime="2009-10-14T16:12:40Z"`) {
		t.Fatal(got)
	}
	if got := string(adminTime(0)); got != "—" {
		t.Fatal(got)
	}
}
