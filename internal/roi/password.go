package roi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// HashPassword creates a PHC-encoded Argon2id hash using a fresh random salt.
func HashPassword(password []byte) (string, error) {
	return hashPasswordContext(context.Background(), password)
}
func hashPasswordContext(ctx context.Context, password []byte) (string, error) {
	if len(password) < 12 || len(password) > 4096 {
		return "", fmt.Errorf("password must contain 12 to 4096 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := derivePasswordContext(ctx, password, salt, 3, 64*1024, 4)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var argonSlots = make(chan struct{}, 2)

func derivePasswordContext(ctx context.Context, password, salt []byte, rounds, memory uint32, threads uint8) ([]byte, error) {
	select {
	case argonSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-argonSlots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := argon2.IDKey(password, salt, rounds, memory, threads, 32)
	return key, ctx.Err()
}

type argonParams struct {
	memory, rounds uint32
	threads        uint8
	salt, key      []byte
}

func parseArgon(hash string) (p argonParams, err error) {
	invalid := fmt.Errorf("invalid Argon2id password_hash")
	if len(hash) > 1024 {
		return p, invalid
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return p, invalid
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return p, invalid
	}
	values := make([]uint64, 3)
	for i, prefix := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(params[i], prefix) {
			return p, invalid
		}
		values[i], err = strconv.ParseUint(strings.TrimPrefix(params[i], prefix), 10, 32)
		if err != nil {
			return p, invalid
		}
	}
	// Bound work and allocations even when configuration is malformed.
	if values[0] < 8*values[2] || values[0] > 256*1024 || values[1] < 1 || values[1] > 10 || values[2] < 1 || values[2] > 16 {
		return p, invalid
	}
	p.memory, p.rounds, p.threads = uint32(values[0]), uint32(values[1]), uint8(values[2])
	p.salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(p.salt) < 16 || len(p.salt) > 64 {
		return p, invalid
	}
	p.key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(p.key) != 32 {
		return p, invalid
	}
	return p, nil
}
func validatePasswordHash(hash string) error {
	if strings.HasPrefix(hash, "$argon2id$") {
		_, err := parseArgon(hash)
		return err
	}
	// Read existing configurations during migration; all newly generated hashes use Argon2id.
	_, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return fmt.Errorf("invalid password_hash (expected Argon2id or legacy bcrypt): %w", err)
	}
	return nil
}
func VerifyPassword(hash string, password []byte) bool {
	ok, _ := VerifyPasswordContext(context.Background(), hash, password)
	return ok
}
func VerifyPasswordContext(ctx context.Context, hash string, password []byte) (bool, error) {
	if len(password) > 4096 {
		return false, nil
	}
	if strings.HasPrefix(hash, "$argon2id$") {
		p, err := parseArgon(hash)
		if err != nil {
			return false, nil
		}
		key, err := derivePasswordContext(ctx, password, p.salt, p.rounds, p.memory, p.threads)
		if err != nil {
			return false, err
		}
		return subtle.ConstantTimeCompare(key, p.key) == 1, nil
	}
	select {
	case argonSlots <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-argonSlots }()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	valid := bcrypt.CompareHashAndPassword([]byte(hash), password) == nil
	return valid, ctx.Err()
}
