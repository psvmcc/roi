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
	if !checkCredentials(c.AdminAuth, "admin", "roi-admin-test") || !checkCredentials(c.APIAuth, "reader", "roi-reader-test") {
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
