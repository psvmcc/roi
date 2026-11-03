package roi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type sessionContextKey struct{}
type requestIdentity struct{ ID string }

func identifySession(r *http.Request, id string) {
	if identity, ok := r.Context().Value(sessionContextKey{}).(*requestIdentity); ok {
		identity.ID = id
	}
}
func sessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type SessionInfo struct {
	ID, Realm, FirstIP, LastIP, Client, State, DeviceName string
	Approved                                              bool
	FeedsHidden                                           bool
	Created, Expires, LastSeen, Requests                  int64
	Current                                               bool
}

func (a *App) createSession(token, realm string, expires time.Time, r *http.Request) error {
	ip, client := "", ""
	if r != nil {
		ip = a.clientIP(r)
		client = logText(r.UserAgent())
	}
	expiresAt := int64(0)
	if !expires.IsZero() {
		expiresAt = expires.Unix()
	}
	csrf := ""
	if realm == "admin" {
		csrf = randomToken()
	}
	tx, err := a.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if realm == "admin" {
		// Keep the 99 most recently used active browsers, then add this one.
		if _, err = tx.Exec("UPDATE auth_sessions SET revoked=1 WHERE id IN (SELECT id FROM auth_sessions WHERE realm='admin' AND revoked=0 AND expires>? ORDER BY last_seen DESC,created DESC,id LIMIT -1 OFFSET 99)", time.Now().Unix()); err != nil {
			return err
		}
	}
	if realm == "api" {
		// Pending tokens have no data access. Bound this queue without ever
		// expiring or evicting an approved Reader client.
		if _, err = tx.Exec("UPDATE auth_sessions SET revoked=1 WHERE id IN (SELECT id FROM auth_sessions WHERE realm='api' AND approved=0 AND revoked=0 ORDER BY last_seen DESC,created DESC,id LIMIT -1 OFFSET 99)"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO auth_sessions(id,realm,created,expires,last_seen,first_ip,last_ip,client,csrf) VALUES(?,?,?,?,?,?,?,?,?)`, sessionID(token), realm, time.Now().Unix(), expiresAt, time.Now().Unix(), ip, ip, client, csrf); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) sessionActive(id string) bool {
	var active bool
	err := a.store.DB.QueryRow("SELECT revoked=0 AND (expires>? OR (realm='api' AND expires=0)) FROM auth_sessions WHERE id=?", time.Now().Unix(), id).Scan(&active)
	return err == nil && active
}
func (a *App) touchSession(r *http.Request, id string) {
	identifySession(r, id)
	_, err := a.store.DB.Exec(`UPDATE auth_sessions SET last_seen=?,last_ip=?,first_ip=CASE WHEN first_ip='' THEN ? ELSE first_ip END,client=?,requests=requests+1 WHERE id=?`, time.Now().Unix(), a.clientIP(r), a.clientIP(r), logText(r.UserAgent()), id)
	if err != nil {
		logAuditError(err)
	}
}
func (a *App) revokeSession(id string) error {
	result, err := a.store.DB.Exec("UPDATE auth_sessions SET revoked=1 WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return clientError("unknown session")
	}
	return nil
}
func (a *App) sessionList(current string) ([]SessionInfo, error) {
	return a.querySessions(current, "", 0)
}
func (a *App) querySessions(current, realm string, page int) ([]SessionInfo, error) {
	query := `SELECT id,realm,created,expires,last_seen,first_ip,last_ip,client,requests,CASE WHEN realm='api' AND approved=0 THEN 'Pending approval' ELSE 'Active' END,device_name,approved,feeds_hidden FROM auth_sessions WHERE revoked=0 AND (expires>? OR (realm='api' AND expires=0))`
	args := []any{time.Now().Unix()}
	if realm != "" {
		query += " AND realm=?"
		args = append(args, realm)
	}
	query += " ORDER BY CASE WHEN realm='api' AND approved=0 THEN 0 ELSE 1 END,last_seen DESC,id"
	if page > 0 {
		query += " LIMIT 100 OFFSET ?"
		args = append(args, (page-1)*100)
	}
	rows, err := a.store.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []SessionInfo{}
	for rows.Next() {
		var s SessionInfo
		if err = rows.Scan(&s.ID, &s.Realm, &s.Created, &s.Expires, &s.LastSeen, &s.FirstIP, &s.LastIP, &s.Client, &s.Requests, &s.State, &s.DeviceName, &s.Approved, &s.FeedsHidden); err != nil {
			return nil, err
		}
		s.Current = s.ID == current
		list = append(list, s)
	}
	return list, rows.Err()
}

// Security events are kept independently of ordinary request logs. The existing
// auth_events table stores only security metadata; old request rows are filtered.
const securityEventSQL = `status IN (401,403,429) OR (method='POST' AND (path='/login' OR path LIKE '%/accounts/ClientLogin' OR path IN ('/approve-session','/device-feeds','/revoke-session','/revoke-api-tokens','/revoke-browser-sessions','/logout')))`

type SecurityEvent struct {
	APIRequest
	Kind string
}

func securityEventKind(e APIRequest) string {
	if e.Status == 429 {
		return "Rate limit exceeded"
	}
	login := e.Method == "POST" && (e.Realm == "admin" && e.Path == "/login" || e.Realm == "api" && strings.HasSuffix(e.Path, "/accounts/ClientLogin"))
	if login {
		if e.Status == 403 {
			return "Login failed"
		}
		if e.Status >= 500 {
			return "Login unavailable"
		}
		if e.Status >= 400 {
			return "Login request rejected"
		}
		if e.Realm == "api" {
			return "Device awaiting approval"
		}
		return "Login succeeded"
	}
	if e.Status == 401 {
		return "Authentication rejected"
	}
	if e.Status == 403 {
		return "Access denied"
	}
	if e.Method != "POST" {
		return ""
	}
	switch e.Path {
	case "/approve-session", "/device-feeds", "/revoke-session", "/revoke-api-tokens", "/revoke-browser-sessions", "/logout":
		if e.Status >= 400 {
			return "Security action failed"
		}
		switch e.Path {
		case "/approve-session":
			return "Device approved"
		case "/device-feeds":
			return "Device feed visibility changed"
		case "/revoke-session":
			return "Session revoked"
		case "/revoke-api-tokens":
			return "API tokens revoked"
		case "/revoke-browser-sessions":
			return "Browser sessions revoked"
		default:
			return "Signed out"
		}
	}
	return ""
}
func (a *App) securityHistory() ([]SecurityEvent, error) {
	rows, err := a.store.DB.Query("SELECT created,realm,session_id,method,path,ip,client,status,device_name,device_state,target_session FROM auth_events WHERE " + securityEventSQL + " ORDER BY id DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []SecurityEvent{}
	for rows.Next() {
		var event SecurityEvent
		if err = rows.Scan(&event.Time, &event.Realm, &event.Session, &event.Method, &event.Path, &event.IP, &event.Client, &event.Status, &event.Device, &event.DeviceState, &event.TargetSession); err != nil {
			return nil, err
		}
		event.Kind = securityEventKind(event.APIRequest)
		list = append(list, event)
	}
	return list, rows.Err()
}
func (a *App) logAdmin(next http.Handler) http.Handler { return a.auditHandler("admin", next) }
func (a *App) auditHandler(realm string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		identity := &requestIdentity{}
		r = r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, identity))
		rw := &apiResponseWriter{ResponseWriter: w}
		defer func() {
			if realm == "admin" && rw.status < 400 && (r.URL.Path == "/assets/admin.css" || r.URL.Path == "/assets/admin.js" || r.URL.Path == "/assets/theme.js" || r.URL.Path == "/assets/favicon.svg" || r.URL.Path == "/favicon.ico" || (r.Method == "GET" && r.URL.Query().Get("fragment") == "1")) {
				return
			}
			status := rw.status
			if status == 0 {
				status = 200
			}
			event := APIRequest{Time: time.Now().Unix(), Realm: realm, Session: identity.ID, IP: a.clientIP(r), Method: r.Method, Path: logText(r.URL.Path), Client: logText(r.UserAgent()), Status: status, Bytes: rw.bytes, Duration: time.Since(started).Milliseconds()}
			a.enrichEvent(&event, r)
			a.recordEvent(event)
		}()
		next.ServeHTTP(rw, r)
	})
}
func (a *App) recordEvent(event APIRequest) {
	if securityEventKind(event) != "" && a.auditGate.allow(event, time.Now()) {
		_, err := a.store.DB.Exec(`INSERT INTO auth_events(created,realm,session_id,method,path,ip,client,status,device_name,device_state,target_session) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, event.Time, event.Realm, event.Session, event.Method, event.Path, event.IP, event.Client, event.Status, event.Device, event.DeviceState, event.TargetSession)
		if err != nil {
			logAuditError(err)
		}
	} else if securityEventKind(event) != "" {
		a.metrics.GetOrCreateCounter("roi_security_events_suppressed_total").Inc()
	}
	if a.config.Debug {
		logAuditEvent(event)
	}
}
func shortSession(id string) string {
	if id == "" {
		return "—"
	}
	return fmt.Sprintf("%s…", id[:min(12, len(id))])
}

// Browser and Reader sessions survive restarts while their respective configured
// credentials remain unchanged. Credential changes revoke only the relevant realm.
func (s *Store) initializeSessions(c Config) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range []struct {
		realm       string
		credentials Credentials
	}{{"api", c.APIAuth}, {"admin", c.AdminAuth}} {
		digest := sessionID(entry.credentials.Username + "\x00" + entry.credentials.PasswordHash)
		name := entry.realm + "_credentials"
		if _, err = tx.Exec("UPDATE auth_sessions SET revoked=1 WHERE realm=? AND (COALESCE((SELECT value FROM auth_secrets WHERE name=?),'')<>? OR (realm='admin' AND csrf=''))", entry.realm, name, digest); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO auth_secrets(name,value) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value", name, digest); err != nil {
			return err
		}
	}
	// Preserve currently active Reader clients across the no-expiry upgrade.
	if _, err = tx.Exec("UPDATE auth_sessions SET expires=0 WHERE realm='api' AND revoked=0 AND expires>?", time.Now().Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM auth_events WHERE NOT (" + securityEventSQL + ")"); err != nil {
		return err
	}
	return tx.Commit()
}

type browserSession struct {
	CSRF    string
	Expires time.Time
}

func (a *App) loadBrowserSession(token string) (browserSession, error) {
	var session browserSession
	if len(token) != 64 {
		return session, sql.ErrNoRows
	}
	if _, err := hex.DecodeString(token); err != nil {
		return session, sql.ErrNoRows
	}
	var expires int64
	err := a.store.DB.QueryRow("SELECT csrf,expires FROM auth_sessions WHERE id=? AND realm='admin' AND revoked=0 AND expires>? AND csrf<>''", sessionID(token), time.Now().Unix()).Scan(&session.CSRF, &expires)
	if err != nil {
		return session, err
	}
	if len(session.CSRF) != 64 {
		return browserSession{}, sql.ErrNoRows
	}
	if _, err = hex.DecodeString(session.CSRF); err != nil {
		return browserSession{}, sql.ErrNoRows
	}
	session.Expires = time.Unix(expires, 0)
	return session, nil
}

func (a *App) sessionApproved(id string) (bool, error) {
	approved, _, err := a.readerAccess(id)
	return approved, err
}
func (a *App) readerAccess(id string) (bool, bool, error) {
	var approved bool
	var hidden bool
	err := a.store.DB.QueryRow("SELECT approved,feeds_hidden FROM auth_sessions WHERE id=? AND realm='api' AND revoked=0 AND (expires=0 OR expires>?)", id, time.Now().Unix()).Scan(&approved, &hidden)
	return approved, hidden, err
}
func (a *App) setDeviceFeedsHidden(id string, hidden bool) error {
	result, err := a.store.DB.Exec("UPDATE auth_sessions SET feeds_hidden=? WHERE id=? AND realm='api' AND approved=1 AND revoked=0 AND (expires=0 OR expires>?)", hidden, id, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return clientError("unknown or unapproved Reader device")
	}
	return nil
}

// Only an authenticated admin can approve a Reader session. Renaming a pending
// device never grants access; approval and its required device name are atomic.
func (a *App) nameSession(id, name string, approve bool) error {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || len([]rune(name)) < 1 || len([]rune(name)) > 128 || strings.ContainsFunc(name, unicode.IsControl) {
		return clientError("device name must contain 1 to 128 characters without control characters")
	}
	result, err := a.store.DB.Exec("UPDATE auth_sessions SET device_name=?,approved=CASE WHEN ? THEN 1 ELSE approved END WHERE id=? AND realm='api' AND revoked=0 AND (expires=0 OR expires>?)", name, approve, id, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return clientError("unknown or inactive Reader session")
	}
	return nil
}

func (a *App) enrichEvent(event *APIRequest, r *http.Request) {
	id := event.Session
	if r.Method == "POST" && (r.URL.Path == "/approve-session" || r.URL.Path == "/device-feeds" || r.URL.Path == "/rename-device" || r.URL.Path == "/revoke-session") {
		target := r.PostForm.Get("session")
		if len(target) == 64 {
			id = target
			event.TargetSession = target
		}
	}
	if event.Realm == "api" {
		event.Device = "unapproved"
		event.DeviceState = "unapproved"
	}
	if id == "" {
		return
	}
	var name, realm string
	var approved bool
	if err := a.store.DB.QueryRow("SELECT device_name,approved,realm FROM auth_sessions WHERE id=?", id).Scan(&name, &approved, &realm); err != nil {
		return
	}
	if realm == "admin" {
		event.Device = "browser"
		event.DeviceState = "browser"
		return
	}
	if approved {
		event.Device = name
		event.DeviceState = "approved"
	} else {
		event.Device = "unapproved"
		event.DeviceState = "unapproved"
	}
}
func (a *App) revokeBrowsers() error {
	if _, err := a.store.DB.Exec("UPDATE auth_sessions SET revoked=1 WHERE realm='admin'"); err != nil {
		return err
	}
	return nil
}
