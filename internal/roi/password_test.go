package roi

import (
	"strings"
	"testing"
)

func TestArgonPasswordsAndBounds(t *testing.T) {
	password := []byte("a long and unique password")
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatal(hash)
	}
	if err = validatePasswordHash(hash); err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, password) || VerifyPassword(hash, []byte("wrong password")) {
		t.Fatal("verification incorrect")
	}
	second, err := HashPassword(password)
	if err != nil || second == hash {
		t.Fatal("salt was reused", err)
	}
	for _, password := range []string{"short", strings.Repeat("x", 4097)} {
		if _, err := HashPassword([]byte(password)); err == nil {
			t.Fatal("invalid length accepted")
		}
	}
	if VerifyPassword(hash, []byte(strings.Repeat("x", 4097))) {
		t.Fatal("overlong password accepted")
	}
	for _, bad := range []string{"", "bad", strings.Repeat("x", 1025), strings.Replace(hash, "v=19", "v=16", 1), strings.Replace(hash, "m=65536", "m=999999999", 1), strings.Replace(hash, "t=3", "t=0", 1), strings.Replace(hash, "p=4", "p=0", 1), strings.Replace(hash, "p=4", "p=17", 1), strings.Replace(hash, "m=65536", "m=1", 1), strings.Replace(hash, "m=65536", "x=65536", 1), strings.Replace(hash, "t=3", "t=11", 1), strings.Replace(hash, "t=3", "t=nan", 1), strings.Replace(hash, "m=65536,t=3,p=4", "m=65536,t=3", 1), strings.Replace(hash, strings.Split(hash, "$")[4], "!!!", 1), hash + "$x", hash[:strings.LastIndex(hash, "$")] + "$!!!"} {
		if validatePasswordHash(bad) == nil || VerifyPassword(bad, password) {
			t.Fatal("malformed hash accepted", bad)
		}
	}
	c := testConfig(t)
	c.AdminAuth.PasswordHash = hash
	c.APIAuth.PasswordHash = hash
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !checkCredentials(c.AdminAuth, c.AdminAuth.Username, string(password)) || checkCredentials(c.AdminAuth, "wrong", string(password)) {
		t.Fatal("credential verification failed")
	}
}
