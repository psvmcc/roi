package roi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"time"
)

func syncErrorCategory(err error, status int, stage string) string {
	var size *gitSizeError
	if errors.As(err, &size) {
		return "Size limit"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "DNS"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "Timeout"
	}
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &authority) || errors.As(err, &hostname) || strings.Contains(strings.ToLower(err.Error()), "tls handshake") {
		return "TLS"
	}
	if status != 0 && (status < 200 || status >= 300 && status != 304) {
		return "HTTP"
	}
	var compression *gitCompressionError
	if errors.As(err, &compression) {
		return "Network"
	}
	if stage == "parse" {
		return "Parse"
	}
	if stage == "storage" {
		return "Storage"
	}
	return "Network"
}
func (p *Poller) hostWaitReason(host string) (string, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.stateLocked(host)
	now := time.Now()
	if now.Before(state.blocked) {
		reason := state.reason
		if reason == "" {
			reason = "Rate limit / Retry-After"
		}
		return reason, state.blocked.Unix()
	}
	if state.active >= p.config.hostConcurrency() {
		return "Another subscription is syncing", 0
	}
	if now.Before(state.ready) {
		return "Per-host delay", state.ready.Unix()
	}
	return "", 0
}
