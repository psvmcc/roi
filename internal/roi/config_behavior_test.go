package roi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationValidationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"listener", func(c *Config) { c.API.Listen = "invalid" }}, {"duplicate listener", func(c *Config) { c.Admin.Listen = c.API.Listen }},
		{"admin prefix", func(c *Config) { c.Admin.Prefix = "/no" }}, {"ops prefix", func(c *Config) { c.Ops.Prefix = "/no" }},
		{"relative prefix", func(c *Config) { c.API.Prefix = "relative" }}, {"slash prefix", func(c *Config) { c.API.Prefix = "/" }}, {"query prefix", func(c *Config) { c.API.Prefix = "/api?x=1" }}, {"fragment prefix", func(c *Config) { c.API.Prefix = "/api#x" }}, {"encoded prefix", func(c *Config) { c.API.Prefix = "/a%2fb" }}, {"escape prefix", func(c *Config) { c.API.Prefix = "/a/../b" }}, {"bad escape", func(c *Config) { c.API.Prefix = "/%zz" }},
		{"admin username", func(c *Config) { c.AdminAuth.Username = "" }}, {"api hash", func(c *Config) { c.APIAuth.PasswordHash = "invalid" }},
		{"empty database", func(c *Config) { c.Database = "" }}, {"memory database", func(c *Config) { c.Database = ":memory:" }},
		{"short interval", func(c *Config) { c.Poll.Interval = "1ms" }}, {"long timeout", func(c *Config) { c.Poll.Timeout = "721h" }}, {"invalid interval", func(c *Config) { c.Poll.Interval = "unknown" }},
		{"zero workers", func(c *Config) { c.Poll.Workers = 0 }}, {"many workers", func(c *Config) { c.Poll.Workers = 33 }}, {"small body", func(c *Config) { c.Poll.MaxBytes = 1 }}, {"large body", func(c *Config) { c.Poll.MaxBytes = 65 << 20 }},
		{"negative months", func(c *Config) { c.Retention.Months = -1 }}, {"negative count", func(c *Config) { c.Retention.PerFeed = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(&c)
			if e := c.Validate(); e == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	c := testConfig(t)
	c.API.Prefix = ""
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
}
func TestConfigurationReadLimitsAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if _, e := LoadConfig(p); e == nil {
		t.Fatal("missing config accepted")
	}
	for _, raw := range []string{"#" + strings.Repeat("x", 1<<20), "api: [invalid", "null", ""} {
		if e := os.WriteFile(p, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadConfig(p); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	c := testConfig(t)
	raw := "admin_auth:\n  username: admin\n  password_hash: '" + c.AdminAuth.PasswordHash + "'\napi_auth:\n  username: reader\n  password_hash: '" + c.APIAuth.PasswordHash + "'\n"
	if e := os.WriteFile(p, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	got, e := LoadConfig(p)
	if e != nil || got.Poll.Interval != "30m" || got.Poll.MaxBytes != 32<<20 || !got.Retention.KeepStarred || got.API.Prefix != "/greader" {
		t.Fatal(got, e)
	}
}
