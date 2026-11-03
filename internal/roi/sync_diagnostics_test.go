package roi

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSyncErrorCategoriesAndHostReasons(t *testing.T) {
	for _, tc := range []struct {
		err         error
		status      int
		stage, want string
	}{
		{&net.DNSError{Err: "not found"}, 0, "network", "DNS"},
		{context.DeadlineExceeded, 0, "network", "Timeout"},
		{x509.UnknownAuthorityError{}, 0, "network", "TLS"},
		{errors.New("HTTP 503"), 503, "network", "HTTP"},
		{errors.New("invalid XML"), 200, "parse", "Parse"},
		{errors.New("disk full"), 200, "storage", "Storage"},
		{errors.New("connection reset"), 0, "network", "Network"},
	} {
		if got := syncErrorCategory(tc.err, tc.status, tc.stage); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
	a := testApp(t)
	p := a.poller
	if reason, _ := p.hostWaitReason("example.org"); reason != "" {
		t.Fatal(reason)
	}
	p.reserveHost("example.org")
	if reason, _ := p.hostWaitReason("example.org"); reason != "Another subscription is syncing" {
		t.Fatal(reason)
	}
	p.releaseHost("example.org")
	p.mu.Lock()
	p.stateLocked("example.org").ready = time.Now().Add(time.Second)
	p.mu.Unlock()
	if reason, until := p.hostWaitReason("example.org"); reason != "Per-host delay" || until == 0 {
		t.Fatal(reason, until)
	}
	p.observeHostResponse(t.Context(), "example.org", &http.Response{StatusCode: 429, Header: http.Header{}})
	if reason, until := p.hostWaitReason("example.org"); reason != "HTTP 429 rate limit" || until <= time.Now().Unix() {
		t.Fatal(reason, until)
	}
}
