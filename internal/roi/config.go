package roi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Credentials struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}
type Endpoint struct {
	Listen    string `yaml:"listen"`
	Prefix    string `yaml:"prefix,omitempty"`
	PublicURL string `yaml:"public_url,omitempty"`
}
type Config struct {
	Backup BackupConfig `yaml:"backup"`
	Git    struct {
		Tokens map[string]string `yaml:"tokens"`
	} `yaml:"git"`
	Debug     bool        `yaml:"debug"`
	API       Endpoint    `yaml:"api"`
	Admin     Endpoint    `yaml:"admin"`
	Ops       Endpoint    `yaml:"ops"`
	Database  string      `yaml:"database"`
	AdminAuth Credentials `yaml:"admin_auth"`
	APIAuth   Credentials `yaml:"api_auth,omitempty"` // Migration only; never accepts new logins.
	Poll      struct {
		Interval           string   `yaml:"interval"`
		PerHostConcurrency int      `yaml:"per_host_concurrency"`
		PerHostDelay       string   `yaml:"per_host_delay"`
		Timeout            string   `yaml:"timeout"`
		Workers            int      `yaml:"workers"`
		MaxBytes           int64    `yaml:"max_bytes"`
		AllowPrivate       bool     `yaml:"allow_private_networks"`
		AllowedNetworks    []string `yaml:"allowed_networks"`
	} `yaml:"poll"`
	Retention struct {
		Months      int  `yaml:"months"`
		PerFeed     int  `yaml:"entries_per_feed"`
		KeepStarred bool `yaml:"keep_starred"`
	} `yaml:"retention"`
	SecureCookies bool           `yaml:"secure_cookies"`
	Security      SecurityConfig `yaml:"security"`
}

type SecurityConfig struct {
	TrustedProxies        []string `yaml:"trusted_proxies"`
	RealIPHeader          string   `yaml:"real_ip_header"`
	FilePermissions       string   `yaml:"file_permissions"`
	ParseConcurrency      int      `yaml:"parse_concurrency"`
	ParseMemoryBytes      int64    `yaml:"parse_memory_bytes"`
	TokenTTL              string   `yaml:"token_ttl"` // Deprecated: accepted for older configs, ignored.
	MaxResponseBytes      int64    `yaml:"max_response_bytes"`
	LoginPerMinute        int      `yaml:"login_per_minute"`
	AccountLoginPerMinute int      `yaml:"account_login_per_minute"`
	APIRequestsPerMinute  int      `yaml:"api_requests_per_minute"`
	APIBurst              int      `yaml:"api_burst"`
	APIConcurrency        int      `yaml:"api_concurrency"`
}

func (c Config) security() SecurityConfig {
	s := c.Security
	if s.FilePermissions == "" {
		s.FilePermissions = "warn"
	}
	if s.ParseConcurrency == 0 {
		s.ParseConcurrency = 2
	}
	if s.ParseMemoryBytes == 0 {
		s.ParseMemoryBytes = 128 << 20
	}
	if s.MaxResponseBytes == 0 {
		s.MaxResponseBytes = 64 << 20
	}
	if s.LoginPerMinute == 0 {
		s.LoginPerMinute = 10
	}
	if s.AccountLoginPerMinute == 0 {
		s.AccountLoginPerMinute = 60
	}
	if s.APIRequestsPerMinute == 0 {
		s.APIRequestsPerMinute = 6000
	}
	if s.APIBurst == 0 {
		s.APIBurst = 200
	}
	if s.APIConcurrency == 0 {
		s.APIConcurrency = 4
	}
	return s
}

func LoadConfig(path string) (Config, error) {
	var c Config
	c.API.Listen = "127.0.0.1:8080"
	c.API.Prefix = "/greader"
	c.Admin.Listen = "127.0.0.1:8081"
	c.Ops.Listen = "127.0.0.1:8082"
	c.Database = "roi.db"
	c.Poll.Interval = "30m"
	c.Poll.Timeout = "30s"
	c.Poll.Workers = 4
	c.Poll.MaxBytes = 32 << 20
	c.Retention.KeepStarred = true
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, errors.New("configuration exceeds 1 MiB")
	}
	if err = checkYAMLDocument(data); err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, errors.New("config must contain one YAML document")
	}
	if err = checkPrivateFile(path, c.security().FilePermissions); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if err := c.backupConfig().validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range []Endpoint{c.API, c.Admin, c.Ops} {
		if _, _, err := net.SplitHostPort(e.Listen); err != nil {
			return fmt.Errorf("listen %q: %w", e.Listen, err)
		}
		if seen[e.Listen] {
			return errors.New("listeners must differ")
		}
		seen[e.Listen] = true
	}
	if c.Admin.Prefix != "" || c.Ops.Prefix != "" {
		return errors.New("prefix is supported only for api")
	}
	if c.Admin.PublicURL != "" || c.Ops.PublicURL != "" {
		return errors.New("public_url is supported only for api")
	}
	if c.API.PublicURL != "" {
		u, err := url.Parse(c.API.PublicURL)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.API.PublicURL, "#") || u.Path != "" && u.Path != "/" || u.RawPath != "" {
			return errors.New("api.public_url must be an absolute http(s) origin without credentials, path, query or fragment; api.prefix is appended automatically")
		}
		if port := u.Port(); port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return errors.New("api.public_url has an invalid port")
			}
		}
	}
	p := c.API.Prefix
	if p != "" {
		u, err := url.Parse(p)
		if err != nil || !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "//") || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
			return errors.New("api.prefix must be empty or an absolute path without trailing slash")
		}
	}
	credentials := []Credentials{c.AdminAuth}
	if c.APIAuth.Username != "" || c.APIAuth.PasswordHash != "" {
		credentials = append(credentials, c.APIAuth)
	}
	for _, a := range credentials {
		if a.Username == "" {
			return errors.New("username is required")
		}
		if err := validatePasswordHash(a.PasswordHash); err != nil {
			return err
		}
	}
	if c.Database == "" || c.Database == ":memory:" {
		return errors.New("database must be a file path")
	}
	for _, s := range []string{c.Poll.Interval, c.Poll.Timeout} {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second || d > 30*24*time.Hour {
			return fmt.Errorf("invalid duration %q (1s..720h)", s)
		}
	}
	delay, err := time.ParseDuration(c.Poll.PerHostDelay)
	if c.Poll.PerHostDelay == "" {
		delay = 2 * time.Second
		err = nil
	}
	if err != nil || delay < 0 || delay > time.Hour || c.hostConcurrency() < 1 || c.hostConcurrency() > 32 {
		return errors.New("poll: per_host_concurrency must be 1..32, per_host_delay must be 0s..1h")
	}
	if c.Poll.Workers < 1 || c.Poll.Workers > 32 || c.Poll.MaxBytes < 1024 || c.Poll.MaxBytes > 64<<20 {
		return errors.New("poll: workers must be 1..32, max_bytes 1024..67108864")
	}
	if c.Retention.Months < 0 || c.Retention.PerFeed < 0 {
		return errors.New("retention values must be nonnegative")
	}
	s := c.security()
	if s.FilePermissions != "warn" && s.FilePermissions != "strict" {
		return errors.New("security.file_permissions must be warn or strict")
	}
	if s.ParseConcurrency < 1 || s.ParseConcurrency > 8 || s.ParseMemoryBytes < max(2*c.Poll.MaxBytes, 16<<20) || s.ParseMemoryBytes > 1<<30 {
		return errors.New("invalid parser resource limits: concurrency 1..8, memory at least 16 MiB and twice poll.max_bytes, at most 1 GiB")
	}
	if err := validateRealIPHeader(s.RealIPHeader); err != nil {
		return err
	}
	for _, raw := range c.Poll.AllowedNetworks {
		if _, err := allowedNetwork(raw); err != nil {
			return err
		}
	}

	if s.MaxResponseBytes < 1024 || s.MaxResponseBytes > 256<<20 {
		return errors.New("security.max_response_bytes must be 1024..268435456")
	}
	if s.LoginPerMinute < 1 || s.LoginPerMinute > 1000 || s.AccountLoginPerMinute < 1 || s.AccountLoginPerMinute > 10000 || s.APIRequestsPerMinute < 1 || s.APIRequestsPerMinute > 60000 || s.APIBurst < 1 || s.APIBurst > 10000 || s.APIConcurrency < 1 || s.APIConcurrency > 32 {
		return errors.New("invalid security rate/concurrency limit")
	}
	for _, raw := range s.TrustedProxies {
		if _, err := proxyPrefix(raw); err != nil {
			return err
		}
	}
	return nil
}
func (c Config) interval() time.Duration { d, _ := time.ParseDuration(c.Poll.Interval); return d }
func (c Config) readerAPIURL() string {
	if c.API.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(c.API.PublicURL, "/") + c.API.Prefix
}
func (c Config) timeout() time.Duration { d, _ := time.ParseDuration(c.Poll.Timeout); return d }
