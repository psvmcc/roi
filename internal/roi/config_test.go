package roi

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConfigStrict(t *testing.T) {
	c := testConfig(t)
	b, e := yaml.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadConfig(p); e != nil {
		t.Fatal(e)
	}
	b = bytes.Replace(b, []byte(`database:`), []byte(`typo:`), 1)
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadConfig(p); e == nil {
		t.Fatal("unknown config key accepted")
	}
}

func TestExampleYAMLAndValidation(t *testing.T) {
	c, e := LoadConfig("../../examples/config.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if !checkCredentials(c.AdminAuth, "admin", "roi-admin-test") {
		t.Fatal("example password comments do not match hashes")
	}
	raw, e := os.ReadFile("../../examples/config.yaml")
	if e != nil {
		t.Fatal(e)
	}
	for name, data := range map[string][]byte{
		"unknown nested field": bytes.Replace(raw, []byte("  interval:"), []byte("  typo:"), 1),
		"multiple documents":   append(bytes.Clone(raw), []byte("\n---\ndatabase: other.db\n")...),
		"duplicate keys":       append(bytes.Clone(raw), []byte("\ndatabase: other.db\n")...),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if e := os.WriteFile(p, data, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := LoadConfig(p); e == nil {
				t.Fatal("invalid YAML config accepted")
			}
		})
	}
}

func TestReaderPublicAPIURL(t *testing.T) {
	for _, tc := range []struct{ origin, prefix, want string }{
		{"", "/greader", ""},
		{"https://reader.example.org", "/greader", "https://reader.example.org/greader"},
		{"https://reader.example.org/", "/private-api", "https://reader.example.org/private-api"},
		{"https://reader.example.org", "", "https://reader.example.org"},
		{"http://127.0.0.1:8080/", "/greader", "http://127.0.0.1:8080/greader"},
		{"http://[::1]:8080", "/greader", "http://[::1]:8080/greader"},
	} {
		c := testConfig(t)
		c.API.PublicURL = tc.origin
		c.API.Prefix = tc.prefix
		if err := c.Validate(); err != nil {
			t.Fatal(tc.origin, err)
		}
		if got := c.readerAPIURL(); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
	for _, origin := range []string{"reader.example.org", "//reader.example.org", "ftp://reader.example.org", "https://user:password@reader.example.org", "https://reader.example.org/greader", "https://reader.example.org?key=value", "https://reader.example.org?", "https://reader.example.org#fragment", "https://reader.example.org#", "https:///", "https://reader.example.org:0", "https://reader.example.org:65536", "https://reader.example.org:bad"} {
		c := testConfig(t)
		c.API.PublicURL = origin
		if err := c.Validate(); err == nil {
			t.Fatal("invalid public API origin accepted")
		}
	}
	c := testConfig(t)
	c.Admin.PublicURL = "https://admin.example.org"
	if err := c.Validate(); err == nil {
		t.Fatal("admin public_url accepted")
	}
	c.Admin.PublicURL = ""
	c.Ops.PublicURL = "https://ops.example.org"
	if err := c.Validate(); err == nil {
		t.Fatal("ops public_url accepted")
	}
}
