package roi

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type gitHTTPError struct {
	Status int
	Retry  time.Duration
}

func (e *gitHTTPError) Error() string { return fmt.Sprintf("Git API HTTP %d", e.Status) }

// A budget covers all decompressed responses in one sync or discovery operation.
type gitBudget struct {
	Limit    int64
	Received int64
}
type gitSizeError struct {
	URL                              string
	Limit, Received, ConfiguredLimit int64
}

func (e *gitSizeError) Error() string {
	return fmt.Sprintf("git API download limit exceeded for %s: received at least %d bytes (%.2f MiB) across API responses; operation budget %d bytes (%.2f MiB), poll.max_bytes=%d. Sizes are counted after decompression; pages and commit lookups share the budget", e.URL, e.Received, float64(e.Received)/(1<<20), e.Limit, float64(e.Limit)/(1<<20), e.ConfiguredLimit)
}
func (p *Poller) gitSizeError(raw string, budget *gitBudget) error {
	return &gitSizeError{URL: raw, Limit: budget.Limit, Received: budget.Received, ConfiguredLimit: p.config.Poll.MaxBytes}
}

type gitCompressionError struct {
	URL string
	Err error
}

func (e *gitCompressionError) Error() string {
	return fmt.Sprintf("cannot decompress HTTP response from %s: %v", e.URL, e.Err)
}
func (e *gitCompressionError) Unwrap() error { return e.Err }

func compressedBodyError(err error) bool {
	var corrupt flate.CorruptInputError
	return errors.As(err, &corrupt) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.Is(err, io.ErrUnexpectedEOF)
}

// API responses, discovery and commit lookups share a bounded budget and the
// same DNS/IP checks as ordinary feeds. Redirects cannot forward host credentials.
func (p *Poller) gitGet(ctx context.Context, raw, token string, budget *gitBudget, nextPage ...*bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.timeout())
	defer cancel()
	body, err := p.gitGetAttempt(ctx, raw, token, budget, false, nextPage...)
	var compression *gitCompressionError
	if !errors.As(err, &compression) {
		return body, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.metrics.GetOrCreateCounter("roi_git_identity_retries_total").Inc()
	return p.gitGetAttempt(ctx, raw, token, budget, true, nextPage...)
}

func (p *Poller) gitGetAttempt(ctx context.Context, raw, token string, budget *gitBudget, identity bool, nextPage ...*bool) ([]byte, error) {
	if budget.Received >= budget.Limit {
		return nil, p.gitSizeError(raw, budget)
	}
	u, err := url.Parse(raw)
	if err == nil {
		if retry := p.hostCooldown(hostGroup(u)); retry > 0 {
			return nil, &gitHTTPError{http.StatusTooManyRequests, retry}
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	if identity {
		req.Header.Set("Accept-Encoding", "identity")
	}
	req.Header.Set("Accept", "application/json")
	if q, e := gitwebQuery(req.URL.RawQuery); e == nil && q.Get("p") != "" {
		req.Header.Set("Accept", "text/html, application/xhtml+xml")
	}
	if token != "" {
		if req.URL.Scheme != "https" {
			return nil, fmt.Errorf("git credentials require an HTTPS endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	cacheKey := gitCacheKey(ctx, raw, token)
	cached, cacheErr := p.cachedGit(ctx, cacheKey)
	if cacheErr != nil && !gitCacheMissing(cacheErr) {
		return nil, cacheErr
	}
	if cacheErr == nil {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.Modified != "" {
			req.Header.Set("If-Modified-Since", cached.Modified)
		}
	}
	client := *p.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || next.URL.Host != req.URL.Host || next.URL.Scheme != req.URL.Scheme {
			return fmt.Errorf("git request redirect outside its origin")
		}
		return validFeedURL(next.URL.String())
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	retry := p.observeHostResponse(ctx, hostGroup(req.URL), resp)
	if resp.StatusCode == http.StatusNotModified {
		if cacheErr != nil {
			return nil, fmt.Errorf("git server returned 304 without a cached response")
		}
		budget.Received += int64(len(cached.Body))
		if budget.Received > budget.Limit {
			return nil, p.gitSizeError(raw, budget)
		}
		if len(nextPage) > 0 {
			*nextPage[0] = cached.Next
		}
		if err = p.cacheGit(ctx, cacheKey, cached); err != nil {
			return nil, err
		}
		p.metrics.GetOrCreateCounter("roi_git_http_cache_hits_total").Inc()
		return cached.Body, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &gitHTTPError{resp.StatusCode, retry}
	}
	if encoding := resp.Header.Get("Content-Encoding"); identity && !resp.Uncompressed && encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, &gitCompressionError{raw, fmt.Errorf("server returned %s despite Accept-Encoding: identity", encoding)}
	}
	hasNext := strings.Contains(resp.Header.Get("Link"), `rel="next"`) || resp.Header.Get("X-Next-Page") != ""
	b, err := io.ReadAll(io.LimitReader(resp.Body, budget.Limit-budget.Received+1))
	budget.Received += int64(len(b))
	if budget.Received > budget.Limit {
		return nil, p.gitSizeError(raw, budget)
	}
	if err != nil {
		if resp.Uncompressed && compressedBodyError(err) {
			return nil, &gitCompressionError{raw, err}
		}
		return nil, err
	}
	if len(nextPage) > 0 {
		*nextPage[0] = hasNext
	}
	if err = p.cacheGit(ctx, cacheKey, gitCacheEntry{ETag: resp.Header.Get("ETag"), Modified: resp.Header.Get("Last-Modified"), Body: b, Next: hasNext}); err != nil {
		return nil, err
	}
	return b, nil
}

func (p *Poller) gitToken(s GitSource) string {
	u, _ := url.Parse(s.URL)
	return p.config.Git.Tokens[u.Host]
}
