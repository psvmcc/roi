package roi

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

func newSigningKey() (string, error) {
	var key [32]byte
	if _, e := rand.Read(key[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(key[:]), nil
}

func (s *Store) SigningKey() (string, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var key string
	e = tx.QueryRow("SELECT value FROM auth_secrets WHERE name='api'").Scan(&key)
	if e == sql.ErrNoRows {
		key, e = newSigningKey()
		if e != nil {
			return "", e
		}
		_, e = tx.Exec("INSERT INTO auth_secrets VALUES('api',?)", key)
	}
	if e != nil {
		return "", e
	}
	b, e := hex.DecodeString(key)
	if e != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid persisted API signing key")
	}
	return key, tx.Commit()
}

func (s *Store) RotateSigningKey() (string, error) {
	key, e := newSigningKey()
	if e != nil {
		return "", e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO auth_secrets VALUES('api',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value", key); e != nil {
		return "", e
	}
	if _, e = tx.Exec("UPDATE auth_sessions SET revoked=1 WHERE realm='api'"); e != nil {
		return "", e
	}
	return key, tx.Commit()
}

// The per-account gate bounds database cardinality even under distributed logins.
// Counts survive process restarts and are shared by the admin/API entry points.
func (s *Store) TakeLogin(realm, client string, perIP, perAccount int, now time.Time) (bool, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	window := now.Unix() / 60
	if _, e = tx.Exec("DELETE FROM login_limits WHERE window<>?", window); e != nil {
		return false, e
	}
	keys := []string{"account:" + realm, "client:" + realm + ":" + client}
	limits := []int{perAccount, perIP}
	for i, key := range keys {
		var hits int
		e = tx.QueryRow("SELECT hits FROM login_limits WHERE key=?", key).Scan(&hits)
		if e != nil && e != sql.ErrNoRows {
			return false, e
		}
		if hits >= limits[i] {
			return false, nil
		}
	}
	for _, key := range keys {
		if _, e = tx.Exec("INSERT INTO login_limits VALUES(?,?,1) ON CONFLICT(key) DO UPDATE SET hits=hits+1", key, window); e != nil {
			return false, e
		}
	}
	return true, tx.Commit()
}
