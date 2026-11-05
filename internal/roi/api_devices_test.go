package roi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func personalLogin(t *testing.T, a *App, c APIDeviceCredentials) string {
	t.Helper()
	w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {c.Login}, "Passwd": {c.Code}})
	if w.Code != 200 {
		t.Fatalf("personal login status %d", w.Code)
	}
	_, token, found := strings.Cut(w.Body.String(), "Auth=")
	if !found {
		t.Fatal("missing Auth token")
	}
	return strings.TrimSpace(token)
}

func TestPersonalAPIDeviceEnrollmentApprovalLifecycleAndAudit(t *testing.T) {
	a := testApp(t)
	a.config.API.PublicURL = "https://reader.example.org/"
	feed := addFeed(t, a, "https://example.org/rss")
	save(t, a, feed, &gofeed.Item{GUID: "personal", Title: "Shared article"})
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "device_name": {"iPhone 15"}}
	w := asyncAdmin(a, cookie, "POST", "/api-devices/create", values)
	var response struct {
		Credentials APIDeviceCredentials `json:"credentials"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Code)
	}
	c := response.Credentials
	if c.APIURL != "https://reader.example.org/greader" {
		t.Fatal("missing public API URL")
	}
	if !strings.HasPrefix(c.Login, "device_") || len(c.Code) != 64 || c.Expires <= time.Now().Unix() {
		t.Fatal("invalid enrollment credentials")
	}
	devices, err := a.store.APIDevices(t.Context())
	if err != nil || len(devices) != 1 {
		t.Fatal(devices, err)
	}
	id := devices[0].ID
	var hash string
	if err = a.store.DB.QueryRow("SELECT code_hash FROM api_devices WHERE id=?", id).Scan(&hash); err != nil || !strings.HasPrefix(hash, "$argon2id$") || hash == c.Code || !VerifyPassword(hash, []byte(c.Code)) {
		t.Fatal("code not stored as an Argon2id hash", err)
	}
	page := adminRequest(a, cookie, "GET", "/devices", nil).Body.String()
	if strings.Contains(page, c.Code) || strings.Contains(page, hash) || !strings.Contains(page, c.Login) {
		t.Fatal("secret exposed in dashboard")
	}
	for _, v := range []url.Values{{"Email": {c.Login}, "Passwd": {"wrong"}}, {"Email": {"reader"}, "Passwd": {"test-password-123"}}} {
		if w = apiReq(a, "", "POST", "/accounts/ClientLogin", v); w.Code != 403 {
			t.Fatal("shared/wrong credentials accepted", w.Code)
		}
	}
	token := personalLogin(t, a, c)
	if w = apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {c.Login}, "Passwd": {c.Code}}); w.Code != 403 {
		t.Fatal("code reused", w.Code)
	}
	w = apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"subscriptions":[]`) {
		t.Fatal("pending account exposed feeds")
	}
	if w = apiReq(a, token, "POST", "/reader/api/0/subscription/quickadd", url.Values{"quickadd": {"https://example.org/new"}}); w.Code != 403 {
		t.Fatal("pending mutation allowed")
	}
	if w = apiReq(a, token, "GET", "/reader/api/0/user-info", nil); w.Code != 200 || !strings.Contains(w.Body.String(), c.Login) {
		t.Fatal("personal user-info missing")
	}
	if w = asyncAdmin(a, cookie, "POST", "/approve-session", url.Values{"csrf": {csrf}, "session": {sessionID(token)}, "device_name": {"iPhone"}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "example.org") {
		t.Fatal("approved device has no subscriptions")
	}
	device := url.Values{"csrf": {csrf}, "device": {fmt.Sprint(id)}}
	if w = asyncAdmin(a, cookie, "POST", "/api-devices/code", device); w.Code != 400 {
		t.Fatal("code regenerated without confirmation")
	}
	device.Set("confirm", "device")
	w = asyncAdmin(a, cookie, "POST", "/api-devices/code", device)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Code)
	}
	if response.Credentials.Code == c.Code || response.Credentials.Login != c.Login || !a.validToken(token) {
		t.Fatal("renewal altered login/session")
	}
	if response.Credentials.APIURL != c.APIURL {
		t.Fatal("renewal lost API URL")
	}
	second := personalLogin(t, a, response.Credentials)
	if approved, err := a.sessionApproved(sessionID(second)); err != nil || approved {
		t.Fatal("renewed login bypassed approval")
	}
	if err = a.store.RenameAPIDevice(t.Context(), id, "Personal phone"); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.querySessions("", "api", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Login != c.Login || s.DeviceName != "Personal phone" {
			t.Fatal("rename/login mismatch", s)
		}
	}
	events, err := a.securityHistory()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), c.Code) || strings.Contains(string(raw), response.Credentials.Code) || strings.Contains(string(raw), token) {
		t.Fatal("audit leaked credentials")
	}
	found := false
	for _, e := range events {
		if e.Kind == "API device created" && strings.Contains(e.Device, c.Login) {
			found = true
		}
	}
	if !found {
		t.Fatal("device creation missing audit identity")
	}
	if w = asyncAdmin(a, cookie, "POST", "/api-devices/delete", device); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if a.validToken(token) || a.validToken(second) {
		t.Fatal("deletion retained sessions")
	}
	var entries int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&entries); err != nil || entries != 1 {
		t.Fatal("device deletion affected feed history")
	}
}

func TestPersonalCodeAtomicityConcurrentReuseExpiryAndRecovery(t *testing.T) {
	a := testApp(t)
	c, err := a.store.CreateAPIDevice(t.Context(), "Mac")
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := a.store.APIDevices(t.Context())
	id := devices[0].ID
	mustExec(t, a, "UPDATE api_devices SET code_expires=1 WHERE id=?", id)
	if w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {c.Login}, "Passwd": {c.Code}}); w.Code != 403 {
		t.Fatal("expired code accepted")
	}
	devices, _ = a.store.APIDevices(t.Context())
	if !devices[0].CodeExpired {
		t.Fatal("expiry not reported")
	}
	c, err = a.store.NewAPIDeviceCode(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, `CREATE TRIGGER reject_personal BEFORE INSERT ON auth_sessions BEGIN SELECT RAISE(ABORT,'session write failed'); END`)
	if w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {c.Login}, "Passwd": {c.Code}}); w.Code != 503 {
		t.Fatal("storage failure not reported", w.Code)
	}
	mustExec(t, a, "DROP TRIGGER reject_personal")
	var group sync.WaitGroup
	results := make(chan string, 6)
	failures := make(chan error, 6)
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			r := httptest.NewRequest("POST", "/accounts/ClientLogin", nil)
			token, err := a.loginAPIDevice(t.Context(), c.Login, c.Code, r)
			results <- token
			failures <- err
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	success := 0
	for token := range results {
		if token != "" {
			success++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("one-time code created multiple sessions", success)
	}
	var count int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM auth_sessions WHERE device_id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = a.store.NewAPIDeviceCode(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = a.RotateAPIKey(); err != nil {
		t.Fatal(err)
	}
	devices, _ = a.store.APIDevices(t.Context())
	if devices[0].CodeAvailable {
		t.Fatal("global revocation retained code")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = a.store.CreateAPIDevice(ctx, "Canceled"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = a.store.NewAPIDeviceCode(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPersonalDevicesPersistAndLegacyConfigCanBeRemoved(t *testing.T) {
	a := testApp(t)
	config := a.config
	payload := "v2.0." + randomToken()
	legacyKey := sign(a.signingKey, "roi-api-v2\n"+config.APIAuth.Username+"\n"+config.APIAuth.PasswordHash)
	legacy := payload + "." + sign(legacyKey, payload)
	if err := a.createSession(legacy, "api", time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := a.store.CreateAPIDevice(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	token := personalLogin(t, a, c)
	unused, err := a.store.CreateAPIDevice(t.Context(), "Mac")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.nameSession(sessionID(token), "Phone", true); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	config.APIAuth = Credentials{}
	a, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.validToken(token) || !a.validToken(legacy) {
		t.Fatal("removing deprecated config invalidated existing sessions")
	}
	oldAction := sign(legacyKey, "csrf:"+legacy)
	if !a.validActionToken(oldAction, legacy) || !a.validActionToken(oldAction[:57], legacy) {
		t.Fatal("legacy action token invalidated during migration")
	}
	other := personalLogin(t, a, unused)
	if other == token {
		t.Fatal("different personal logins reused a bearer token")
	}
	sessions, err := a.querySessions("", "api", 1)
	if err != nil {
		t.Fatal(err)
	}
	logins := map[string]bool{}
	for _, s := range sessions {
		if s.Login != "" {
			logins[s.Login] = true
		}
	}
	if !logins[c.Login] || !logins[unused.Login] {
		t.Fatal("personal sessions lost their individual logins")
	}
	if w := apiReq(a, "", "POST", "/accounts/ClientLogin", url.Values{"Email": {c.Login}, "Passwd": {c.Code}}); w.Code != 403 {
		t.Fatal("restart revived consumed code")
	}
	if err = a.RotateAPIKey(); err != nil {
		t.Fatal(err)
	}
	if a.validToken(legacy) || a.validToken(token) {
		t.Fatal("rotation retained legacy or personal session")
	}
}

func TestPersonalDeviceAdminAuthorizationConfirmationAndSecretResponses(t *testing.T) {
	a := testApp(t)
	a.config.API.PublicURL = "https://reader.example.org"
	cookie, csrf := adminSession(t, a)
	values := url.Values{"csrf": {csrf}, "device_name": {"Tablet"}}
	if w := asyncAdmin(a, nil, "POST", "/api-devices/create", values); w.Code != 401 {
		t.Fatal(w.Code)
	}
	bad := cloneValues(values)
	bad.Set("csrf", "invalid")
	if w := asyncAdmin(a, cookie, "POST", "/api-devices/create", bad); w.Code != 403 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/api-devices/create", strings.NewReader(values.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.AdminHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, name := range []string{"", "  ", "bad\nname", strings.Repeat("x", 129)} {
		bad = cloneValues(values)
		bad.Set("device_name", name)
		if w := asyncAdmin(a, cookie, "POST", "/api-devices/create", bad); w.Code != 400 {
			t.Fatal("invalid name accepted", w.Code)
		}
	}
	c, err := a.store.CreateAPIDevice(t.Context(), "Tablet")
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := a.store.APIDevices(t.Context())
	id := devices[0].ID
	form := url.Values{"csrf": {csrf}, "device": {fmt.Sprint(id)}, "confirm": {"device"}}
	for _, route := range []string{"/api-devices/code", "/api-devices/rename", "/api-devices/delete"} {
		bad = cloneValues(form)
		bad.Set("device", "unknown")
		if w := asyncAdmin(a, cookie, "POST", route, bad); w.Code != 400 {
			t.Fatal(route, w.Code)
		}
		bad.Set("device", "99999")
		if w := asyncAdmin(a, cookie, "POST", route, bad); w.Code != 400 {
			t.Fatal(route, w.Code)
		}
	}
	if w := asyncAdmin(a, cookie, "POST", "/api-devices/missing", form); w.Code != 404 {
		t.Fatal(w.Code)
	}
	form.Set("device_name", "Renamed tablet")
	if w := asyncAdmin(a, cookie, "POST", "/api-devices/rename", form); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = adminRequest(a, cookie, "POST", "/api-devices/code", form)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "One-time sign-in credentials") || !strings.Contains(w.Body.String(), c.Login) {
		t.Fatal("one-time HTML response", w.Code)
	}
	if !strings.Contains(w.Body.String(), "https://reader.example.org/greader") {
		t.Fatal("HTML credentials omitted public API URL")
	}
	if w := adminRequest(a, cookie, "GET", "/api-devices/code", nil); w.Code != 404 {
		t.Fatal("GET generated credentials")
	}
	var exported bytes.Buffer
	if err = a.store.ExportROI(&exported); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), c.Login) || strings.Contains(exported.String(), c.Code) {
		t.Fatal("ROI export leaked device access")
	}
	token := approvedToken(t, a)
	if w := apiReq(a, token, "POST", "/reader/api/0/api-devices/create", values); w.Code != 404 {
		t.Fatal("Reader API can create devices")
	}
}

func TestPersonalDeviceStorageFailuresAndLimits(t *testing.T) {
	a := testApp(t)
	c, err := a.store.CreateAPIDevice(t.Context(), "Device")
	if err != nil {
		t.Fatal(err)
	}
	devices, _ := a.store.APIDevices(t.Context())
	id := devices[0].ID
	token := personalLogin(t, a, c)
	mustExec(t, a, `CREATE TRIGGER fail_device_delete BEFORE DELETE ON api_devices BEGIN SELECT RAISE(ABORT,'failure'); END`)
	if err = a.store.DeleteAPIDevice(t.Context(), id); err == nil || !a.validToken(token) {
		t.Fatal("failed deletion was not atomic")
	}
	mustExec(t, a, "DROP TRIGGER fail_device_delete")
	mustExec(t, a, `CREATE TRIGGER fail_device_update BEFORE UPDATE ON api_devices BEGIN SELECT RAISE(ABORT,'failure'); END`)
	if _, err = a.store.NewAPIDeviceCode(t.Context(), id); err == nil {
		t.Fatal("failed update succeeded")
	}
	if err = a.store.RenameAPIDevice(t.Context(), id, "Changed"); err == nil {
		t.Fatal("failed rename succeeded")
	}
	mustExec(t, a, "DROP TRIGGER fail_device_update")
	if _, err = a.store.NewAPIDeviceCode(t.Context(), 9999); err == nil {
		t.Fatal("unknown code target")
	}
	if err = a.store.RenameAPIDevice(t.Context(), 9999, "Name"); err == nil {
		t.Fatal("unknown rename target")
	}
	if err = a.store.RenameAPIDevice(t.Context(), id, ""); err == nil {
		t.Fatal("invalid rename")
	}
	if err = a.store.DeleteAPIDevice(t.Context(), 9999); err == nil {
		t.Fatal("unknown deletion target")
	}
	mustExec(t, a, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<999) INSERT INTO api_devices(login,name,created) SELECT 'fake-'||x,'Placeholder',0 FROM n`)
	if _, err = a.store.CreateAPIDevice(t.Context(), "Too many"); err == nil {
		t.Fatal("unbounded devices")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.APIDevices(t.Context()); err == nil {
		t.Fatal("closed DB listed devices")
	}
	if _, err = a.store.CreateAPIDevice(t.Context(), "Closed"); err == nil {
		t.Fatal("closed DB created device")
	}
	if _, err = a.store.NewAPIDeviceCode(t.Context(), id); err == nil {
		t.Fatal("closed DB renewed code")
	}
	if err = a.store.RenameAPIDevice(t.Context(), id, "Closed"); err == nil {
		t.Fatal("closed DB renamed device")
	}
	if err = a.store.DeleteAPIDevice(t.Context(), id); err == nil {
		t.Fatal("closed DB deleted device")
	}
}
