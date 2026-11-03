package roi

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestForwardedClientIPHeadersAndTrust(t *testing.T) {
	a := testApp(t)
	trusted := []string{"127.0.0.1", "::1", "10.0.0.0/8", "2001:db8:1::/48"}
	for _, tc := range []struct {
		name, peer string
		xff, real  []string
		want       string
		untrusted  bool
	}{
		{name: "real IP", peer: "127.0.0.1:80", real: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{name: "default trust empty", peer: "127.0.0.1:80", real: []string{"203.0.113.9"}, want: "127.0.0.1", untrusted: true},
		{name: "untrusted peer", peer: "198.51.100.1:80", xff: []string{"203.0.113.9"}, real: []string{"203.0.113.8"}, want: "198.51.100.1"},
		{name: "XFF wins", peer: "127.0.0.1:80", xff: []string{"203.0.113.9"}, real: []string{"203.0.113.8"}, want: "203.0.113.9"},
		{name: "rightmost untrusted", peer: "127.0.0.1:80", xff: []string{"192.0.2.1, 203.0.113.9"}, real: []string{"203.0.113.8"}, want: "203.0.113.9"},
		{name: "multiple proxies", peer: "127.0.0.1:80", xff: []string{"203.0.113.9, 10.0.0.2"}, want: "203.0.113.9"},
		{name: "repeated XFF lines", peer: "127.0.0.1:80", xff: []string{"spoofed, 203.0.113.9", "10.0.0.2"}, want: "203.0.113.9"},
		{name: "malformed trusted hop", peer: "127.0.0.1:80", xff: []string{"203.0.113.9", "bad"}, real: []string{"203.0.113.8"}, want: "127.0.0.1"},
		{name: "empty XFF blocks fallback", peer: "127.0.0.1:80", xff: []string{""}, real: []string{"203.0.113.8"}, want: "127.0.0.1"},
		{name: "real IP IPv6", peer: "[::1]:80", real: []string{"2001:db8:2::9"}, want: "2001:db8:2::9"},
		{name: "IPv6 proxy chain", peer: "[2001:db8:1::10]:80", xff: []string{"2001:db8:2::9, ::1"}, want: "2001:db8:2::9"},
		{name: "mapped peer", peer: "[::ffff:127.0.0.1]:80", real: []string{"::ffff:203.0.113.9"}, want: "203.0.113.9"},
		{name: "mapped XFF", peer: "127.0.0.1:80", xff: []string{"::ffff:203.0.113.9, ::ffff:10.0.0.2"}, want: "203.0.113.9"},
		{name: "real IP whitespace", peer: "127.0.0.1:80", real: []string{" 203.0.113.9 "}, want: "203.0.113.9"},
		{name: "multiple real IPs", peer: "127.0.0.1:80", real: []string{"203.0.113.9", "203.0.113.8"}, want: "127.0.0.1"},
		{name: "real IP list", peer: "127.0.0.1:80", real: []string{"203.0.113.9, 203.0.113.8"}, want: "127.0.0.1"},
		{name: "real IP with port", peer: "127.0.0.1:80", real: []string{"203.0.113.9:80"}, want: "127.0.0.1"},
		{name: "real IP scope", peer: "127.0.0.1:80", real: []string{"fe80::1%lo0"}, want: "127.0.0.1"},
		{name: "XFF scope", peer: "127.0.0.1:80", xff: []string{"fe80::1%lo0"}, want: "127.0.0.1"},
		{name: "bad real IP", peer: "127.0.0.1:80", real: []string{"invalid"}, want: "127.0.0.1"},
		{name: "long real IP", peer: "127.0.0.1:80", real: []string{strings.Repeat("x", 257)}, want: "127.0.0.1"},
		{name: "oversized chain", peer: "127.0.0.1:80", xff: []string{strings.Repeat(" ", 4097) + "203.0.113.9"}, want: "127.0.0.1"},
		{name: "too many hops", peer: "127.0.0.1:80", xff: []string{strings.Repeat("10.0.0.2,", 20) + "203.0.113.9"}, want: "127.0.0.1"},
		{name: "bad peer", peer: "bad:80", real: []string{"203.0.113.9"}, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a.config.Security.TrustedProxies = trusted
			if tc.untrusted {
				a.config.Security.TrustedProxies = nil
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			for _, v := range tc.real {
				r.Header.Add("X-Real-IP", v)
			}
			if got := a.clientIP(r); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
}

func TestProxyConfigurationIPsCIDRsAndNormalization(t *testing.T) {
	c := testConfig(t)
	for input, want := range map[string]string{"127.0.0.1": "127.0.0.1/32", "::1": "::1/128", "10.20.0.7/24": "10.20.0.0/24", "2001:db8:1::7/48": "2001:db8:1::/48", "::ffff:127.0.0.1": "127.0.0.1/32", "::ffff:10.20.0.0/120": "10.20.0.0/24", " 127.0.0.1 ": "127.0.0.1/32"} {
		p, err := proxyPrefix(input)
		if err != nil || p.String() != want {
			t.Fatal(input, p, err)
		}
		c.Security.TrustedProxies = []string{input}
		if err := c.Validate(); err != nil {
			t.Fatal(input, err)
		}
	}
	for _, input := range []string{"", "bad", "localhost", "127.0.0.1:80", "0.0.0.0/0", "::/0", "::ffff:0.0.0.0/96", "::ffff:10.0.0.0/80", "fe80::1%lo0", "10.0.0.0/33"} {
		c.Security.TrustedProxies = []string{input}
		if err := c.Validate(); err == nil {
			t.Fatal("invalid trust accepted", input)
		}
	}
	c.Security.TrustedProxies = []string{"127.0.0.1", "::1", "10.20.0.0/24"}
	raw, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || len(loaded.Security.TrustedProxies) != 3 {
		t.Fatal(loaded.Security.TrustedProxies, err)
	}
	a := testApp(t)
	a.config.Security.TrustedProxies = []string{"invalid", "::ffff:10.20.0.0/120"}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.20.0.2:80"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	if got := a.clientIP(r); got != "203.0.113.9" {
		t.Fatal(got)
	}
}

func TestProxyIPUsedForLoginLimitsSessionMetadataAndLogs(t *testing.T) {
	c := testConfig(t)
	c.Security.TrustedProxies = []string{"127.0.0.1"}
	c.Security.LoginPerMinute = 1
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.config.Debug = true
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		ip   string
		want int
	}{{"203.0.113.10", 403}, {"203.0.113.10", 429}, {"203.0.113.11", 403}} {
		values := url.Values{"Email": {"reader"}, "Passwd": {"wrong-password"}}
		r := httptest.NewRequest("POST", "/greader/accounts/ClientLogin", strings.NewReader(values.Encode()))
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Real-IP", tc.ip)
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(tc, w.Code)
		}
	}
	token := approvedToken(t, a)
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		r := httptest.NewRequest("GET", "/greader/reader/api/0/user-info", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set(header, "203.0.113.12")
		r.Header.Set("Authorization", "GoogleLogin auth="+token)
		w := httptest.NewRecorder()
		a.APIHandler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	list, err := a.sessionList("")
	if err != nil || len(list) != 1 || list[0].FirstIP != "203.0.113.12" || list[0].LastIP != "203.0.113.12" {
		t.Fatal(list, err)
	}
	events, err := a.securityHistory()
	if err != nil || len(events) != 3 {
		t.Fatal(events, err)
	}
	if events[0].IP != "203.0.113.11" || events[1].IP != "203.0.113.10" || !strings.Contains(logs.String(), `"ip":"203.0.113.12"`) {
		t.Fatal("proxy IP not propagated", events, logs.String())
	}
}
