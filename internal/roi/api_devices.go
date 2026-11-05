package roi

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const apiSignInTTL = 24 * time.Hour

type APIDevice struct {
	ID, Created, CodeExpires, Sessions int64
	Login, Name                        string
	CodeAvailable, CodeExpired         bool
}

// Credentials are returned only to the authenticated creation/renewal request.
// Neither the plaintext code nor a bearer token is retained in SQLite.
type APIDeviceCredentials struct {
	APIURL  string `json:"api_url,omitempty"`
	Login   string `json:"login"`
	Code    string `json:"code"`
	Expires int64  `json:"expires"`
}

func (s *Store) RenameAPIDevice(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if !validDeviceName(name) {
		return clientError("invalid device name")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE api_devices SET name=? WHERE id=?", name, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return clientError("unknown API device")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE auth_sessions SET device_name=? WHERE realm='api' AND device_id=?", name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func validDeviceName(name string) bool {
	return utf8.ValidString(name) && len([]rune(name)) >= 1 && len([]rune(name)) <= 128 && !strings.ContainsFunc(name, unicode.IsControl)
}

func (s *Store) CreateAPIDevice(ctx context.Context, name string) (APIDeviceCredentials, error) {
	name = strings.TrimSpace(name)
	if !validDeviceName(name) {
		return APIDeviceCredentials{}, clientError("device name must contain 1 to 128 characters without control characters")
	}
	credentials := APIDeviceCredentials{Login: "device_" + randomToken()[:16], Code: randomToken(), Expires: time.Now().Add(apiSignInTTL).Unix()}
	hash, err := hashPasswordContext(ctx, []byte(credentials.Code))
	if err != nil {
		return APIDeviceCredentials{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return APIDeviceCredentials{}, err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM api_devices").Scan(&count); err != nil {
		return APIDeviceCredentials{}, err
	}
	if count >= 1000 {
		return APIDeviceCredentials{}, clientError("maximum of 1000 API devices reached; delete unused devices first")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO api_devices(login,name,code_hash,code_expires,created) VALUES(?,?,?,?,?)", credentials.Login, name, hash, credentials.Expires, time.Now().Unix()); err != nil {
		return APIDeviceCredentials{}, err
	}
	if err = tx.Commit(); err != nil {
		return APIDeviceCredentials{}, err
	}
	return credentials, nil
}

func (s *Store) APIDevices(ctx context.Context) ([]APIDevice, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id,d.login,d.name,d.created,d.code_expires,d.code_hash<>'',
(SELECT COUNT(*) FROM auth_sessions a WHERE a.device_id=d.id AND a.realm='api' AND a.revoked=0 AND (a.expires=0 OR a.expires>?))
FROM api_devices d ORDER BY d.created DESC,d.id DESC`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []APIDevice{}
	for rows.Next() {
		var d APIDevice
		if err = rows.Scan(&d.ID, &d.Login, &d.Name, &d.Created, &d.CodeExpires, &d.CodeAvailable, &d.Sessions); err != nil {
			return nil, err
		}
		d.CodeExpired = d.CodeAvailable && d.CodeExpires <= time.Now().Unix()
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) NewAPIDeviceCode(ctx context.Context, id int64) (APIDeviceCredentials, error) {
	var credentials APIDeviceCredentials
	if err := s.DB.QueryRowContext(ctx, "SELECT login FROM api_devices WHERE id=?", id).Scan(&credentials.Login); err != nil {
		if err == sql.ErrNoRows {
			return credentials, clientError("unknown API device")
		}
		return credentials, err
	}
	credentials.Code, credentials.Expires = randomToken(), time.Now().Add(apiSignInTTL).Unix()
	hash, err := hashPasswordContext(ctx, []byte(credentials.Code))
	if err != nil {
		return APIDeviceCredentials{}, err
	}
	result, err := s.DB.ExecContext(ctx, "UPDATE api_devices SET code_hash=?,code_expires=? WHERE id=?", hash, credentials.Expires, id)
	if err != nil {
		return APIDeviceCredentials{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return APIDeviceCredentials{}, err
	}
	if n == 0 {
		return APIDeviceCredentials{}, clientError("unknown API device")
	}
	return credentials, nil
}

func (s *Store) DeleteAPIDevice(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE auth_sessions SET revoked=1 WHERE realm='api' AND device_id=?", id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM api_devices WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return clientError("unknown API device")
	}
	return tx.Commit()
}

func (a *App) loginAPIDevice(ctx context.Context, login, code string, r *http.Request) (string, error) {
	if len(login) > 128 || len(code) > 4096 {
		return "", nil
	}
	var id, expires int64
	var hash, name string
	err := a.store.DB.QueryRowContext(ctx, "SELECT id,name,code_hash,code_expires FROM api_devices WHERE login=?", login).Scan(&id, &name, &hash, &expires)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if hash == "" || expires <= time.Now().Unix() {
		return "", nil
	}
	valid, err := VerifyPasswordContext(ctx, hash, []byte(code))
	if err != nil || !valid {
		return "", err
	}
	a.keyRotation.Lock()
	defer a.keyRotation.Unlock()
	tx, err := a.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE api_devices SET code_hash='',code_expires=0 WHERE id=? AND code_hash=? AND code_expires>?", id, hash, time.Now().Unix())
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return "", err
	}
	// An admin may have renamed the device while password verification ran.
	if err = tx.QueryRowContext(ctx, "SELECT name FROM api_devices WHERE id=?", id).Scan(&name); err != nil {
		return "", err
	}
	// Keep the existing bounded pending queue; approved sessions are never evicted.
	if _, err = tx.ExecContext(ctx, "UPDATE auth_sessions SET revoked=1 WHERE id IN (SELECT id FROM auth_sessions WHERE realm='api' AND approved=0 AND revoked=0 ORDER BY last_seen DESC,created DESC,id LIMIT -1 OFFSET 99)"); err != nil {
		return "", err
	}
	payload := "v2.0." + randomToken()
	token := payload + "." + sign(a.apiKey(), payload)
	ip, client := a.clientIP(r), logText(r.UserAgent())
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_sessions(id,realm,created,expires,last_seen,first_ip,last_ip,client,device_name,device_id,approved)
VALUES(?,'api',?,0,?,?,?,?,?,?,0)`, sessionID(token), now, now, ip, ip, client, name, id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}
