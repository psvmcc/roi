package roi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestPendingReaderCannotReadOrMutateBeforeApproval(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://private.example/feed")
	save(t, a, id, &gofeed.Item{GUID: "private-guid", Title: "Private article", Content: "Private contents"})
	mustExec(t, a, "UPDATE feeds SET title='Private feed' WHERE id=?", id)
	mustExec(t, a, "INSERT INTO folders VALUES('Private folder')")
	mustExec(t, a, "INSERT INTO feed_folders VALUES(?,'Private folder')", id)
	w := apiReq(a, "", "POST", "/accounts/ClientLogin", deviceLoginForm(t, a))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	token := strings.TrimSpace(strings.Split(w.Body.String(), "Auth=")[1])
	approved, err := a.sessionApproved(sessionID(token))
	if err != nil || approved || !a.validToken(token) {
		t.Fatal("new login is not pending", err)
	}
	assertEmpty := func() {
		t.Helper()
		for route, key := range map[string]string{"subscription/list": "subscriptions", "tag/list": "tags", "unread-count": "unreadcounts", "stream/items/ids": "itemRefs", "stream/items/contents": "items", "stream/contents/reading-list": "items", "stream/contents/feed/1": "items", "stream/contents/user/1/label/Private folder": "items"} {
			values := url.Values{"i": {"1", entryID(1)}, "s": {"feed/1"}, "c": {"guessed-cursor"}}
			path := "/reader/api/0/" + strings.ReplaceAll(route, " ", "%20")
			w := apiReq(a, token, "POST", path, values)
			var result map[string]json.RawMessage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || string(result[key]) != "[]" {
				t.Fatal(route, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "Private article") || strings.Contains(w.Body.String(), "private.example") || result["continuation"] != nil {
				t.Fatal("pending data leak", route, w.Body.String())
			}
		}
	}
	assertEmpty()
	for _, route := range []string{"token", "user-info", "preference/list", "preference/stream/list"} {
		if w := apiReq(a, token, "GET", "/reader/api/0/"+route, nil); w.Code != 200 {
			t.Fatal(route, w.Code)
		}
	}
	for _, route := range []string{"edit-tag", "mark-all-as-read", "subscription/edit", "subscription/quickadd", "rename-tag", "disable-tag"} {
		w := apiReq(a, token, "POST", "/reader/api/0/"+route, url.Values{"T": {"x"}, "s": {"feed/1"}, "i": {"1"}, "a": {statePrefix + "read"}, "ac": {"unsubscribe"}, "quickadd": {"https://other.example/rss"}, "t": {"Changed"}})
		if w.Code != 403 {
			t.Fatal("pending mutation accepted", route, w.Code, w.Body.String())
		}
	}
	if w := apiReq(a, token, "GET", "/reader/api/0/missing", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	var read, active int
	if err := a.store.DB.QueryRow("SELECT e.is_read,f.active FROM entries e JOIN feeds f ON f.id=e.feed_id").Scan(&read, &active); err != nil || read != 0 || active != 1 {
		t.Fatal("pending client changed database", read, active, err)
	}
	cookie, csrf := adminSession(t, a)
	values := url.Values{"session": {sessionID(token)}, "device_name": {"MacBook"}, "csrf": {csrf}}
	if w := asyncAdmin(a, nil, "POST", "/approve-session", values); w.Code != 401 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", "wrong")
	if w := asyncAdmin(a, cookie, "POST", "/approve-session", values); w.Code != 403 {
		t.Fatal(w.Code)
	}
	values.Set("csrf", csrf)
	// Saving a name on its own must never approve a pending client.
	if w := asyncAdmin(a, cookie, "POST", "/rename-device", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertEmpty()
	values.Set("device_name", "")
	if w := asyncAdmin(a, cookie, "POST", "/approve-session", values); w.Code != 400 {
		t.Fatal(w.Code)
	}
	values.Set("device_name", "  Reeder on MacBook  ")
	if w := asyncAdmin(a, cookie, "POST", "/approve-session", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Private feed") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := apiReq(a, token, "POST", "/reader/api/0/stream/items/contents", url.Values{"i": {"1"}}); w.Code != 200 || !strings.Contains(w.Body.String(), "Private article") {
		t.Fatal(w.Code, w.Body.String())
	}
	values.Set("device_name", "Reeder desktop")
	if w := asyncAdmin(a, cookie, "POST", "/rename-device", values); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	page := adminRequest(a, cookie, "GET", "/", nil)
	if !strings.Contains(page.Body.String(), "Reeder desktop") {
		t.Fatal("device name missing")
	}
	next := a.issueToken()
	if ok, err := a.sessionApproved(sessionID(next)); err != nil || ok {
		t.Fatal("new session inherited approval", err)
	}
}

func TestDeviceApprovalValidationAndPersistence(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	token, pending := a.issueToken(), a.issueToken()
	for _, name := range []string{"", "   ", strings.Repeat("a", 129), "line\nbreak", string([]byte{255})} {
		if err := a.nameSession(sessionID(token), name, true); err == nil {
			t.Fatal("invalid name accepted")
		}
	}
	cookie, _ := adminSession(t, a)
	for _, id := range []string{"missing", sessionID(cookie.Value)} {
		if err := a.nameSession(id, "Device", true); err == nil {
			t.Fatal("invalid session approved")
		}
	}
	if err := a.nameSession(sessionID(token), "My Mac", true); err != nil {
		t.Fatal(err)
	}
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if ok, err := a.sessionApproved(sessionID(token)); err != nil || !ok {
		t.Fatal("approval lost on restart", err)
	}
	if ok, err := a.sessionApproved(sessionID(pending)); err != nil || ok {
		t.Fatal("pending session approved on restart", err)
	}
	all, err := a.sessionList("")
	var list []SessionInfo
	for _, session := range all {
		if session.Realm == "api" {
			list = append(list, session)
		}
	}
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	if list[0].ID != sessionID(pending) || list[0].State != "Pending approval" || list[1].DeviceName != "My Mac" {
		t.Fatal("pending order or name incorrect", list)
	}
	if err := a.revokeSession(sessionID(token)); err != nil {
		t.Fatal(err)
	}
	if err := a.nameSession(sessionID(token), "Device", true); err == nil {
		t.Fatal("revoked session approved")
	}
	mustExec(t, a, "UPDATE auth_sessions SET expires=? WHERE id=?", time.Now().Add(-time.Second).Unix(), sessionID(pending))
	if err := a.nameSession(sessionID(pending), "Device", true); err == nil {
		t.Fatal("expired session approved")
	}
	a.Close()
	if err := a.nameSession(sessionID(pending), "Device", true); err == nil {
		t.Fatal("closed store ignored")
	}
}

func TestApprovalMigrationDefaultsExistingSessionsToPending(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	token := approvedToken(t, a)
	id := addFeed(t, a, "https://example.org/rss")
	save(t, a, id, &gofeed.Item{GUID: "preserved"})
	mustExec(t, a, "ALTER TABLE auth_sessions DROP COLUMN csrf; ALTER TABLE auth_sessions DROP COLUMN approved; ALTER TABLE auth_sessions DROP COLUMN device_name; ALTER TABLE auth_events DROP COLUMN device_name; ALTER TABLE auth_events DROP COLUMN device_state; ALTER TABLE auth_events DROP COLUMN target_session; DROP TRIGGER git_folder_only; ALTER TABLE feeds DROP COLUMN user_agent; ALTER TABLE feeds DROP COLUMN git_provider; ALTER TABLE feeds DROP COLUMN git_repo; ALTER TABLE feeds DROP COLUMN git_mode; ALTER TABLE feeds DROP COLUMN git_preview; ALTER TABLE feeds DROP COLUMN git_revision; ALTER TABLE feeds DROP COLUMN icon_url; ALTER TABLE entries DROP COLUMN git_preview; ALTER TABLE feeds DROP COLUMN git_tag_filter; PRAGMA user_version=3")
	a.Close()
	a, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.validToken(token) {
		t.Fatal("migration lost session")
	}
	if ok, err := a.sessionApproved(sessionID(token)); err != nil || ok {
		t.Fatal("migration granted access", err)
	}
	if w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"subscriptions":[]}` {
		t.Fatal(w.Code, w.Body.String())
	}
	feeds, err := a.store.Feeds()
	if err != nil || len(feeds) != 1 || feeds[0].Count != 1 {
		t.Fatal("migration lost articles", err)
	}
}

func TestApprovalStorageFailuresDoNotGrantAccess(t *testing.T) {
	a := testApp(t)
	token := a.issueToken()
	cookie, csrf := adminSession(t, a)
	mustExec(t, a, `CREATE TRIGGER fail_approval BEFORE UPDATE OF approved ON auth_sessions BEGIN SELECT RAISE(ABORT,'storage unavailable'); END`)
	if w := asyncAdmin(a, cookie, "POST", "/approve-session", url.Values{"session": {sessionID(token)}, "device_name": {"Mac"}, "csrf": {csrf}}); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if ok, err := a.sessionApproved(sessionID(token)); err != nil || ok {
		t.Fatal("failed approval granted access", err)
	}
	mustExec(t, a, "DROP TRIGGER fail_approval")
	mustExec(t, a, "UPDATE auth_sessions SET approved='corrupted' WHERE id=?", sessionID(token))
	if w := apiReq(a, token, "GET", "/reader/api/0/subscription/list", nil); w.Code != 500 {
		t.Fatal("corrupted approval accepted", w.Code)
	}
}
