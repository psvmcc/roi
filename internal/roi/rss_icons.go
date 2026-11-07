package roi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	xhtml "golang.org/x/net/html"
)

type rssIcon struct {
	URL, Site string
	Checked   int64
}

func resolvedIcon(base, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	result := b.ResolveReference(u).String()
	if len(result) > 8192 || validFeedURL(result) != nil {
		return ""
	}
	return result
}
func (p *Poller) rssIcon(ctx context.Context, feed *gofeed.Feed, feedURL, savedSite, override, cached, cachedSite string, checked int64, agent string) rssIcon {
	now := time.Now().Unix()
	site := ""
	if feed != nil {
		site = resolvedIcon(feedURL, feed.Link)
	}
	if override != "" {
		site = resolvedIcon(feedURL, override)
	}
	if site == "" {
		site = resolvedIcon(feedURL, savedSite)
	}
	if site == "" {
		u, err := url.Parse(feedURL)
		if err != nil || validFeedURL(feedURL) != nil {
			return rssIcon{URL: cached, Site: cachedSite, Checked: checked}
		}
		u.Path = "/"
		u.RawPath = ""
		u.RawQuery = ""
		u.Fragment = ""
		site = u.String()
	}
	if feed != nil && feed.Image != nil {
		if image := resolvedIcon(feedURL, feed.Image.URL); image != "" {
			return rssIcon{image, site, now}
		}
	}
	if cached != "" && cachedSite == "" && checked == 0 {
		return rssIcon{cached, site, now}
	}
	if cached != "" && cachedSite == site && (checked > now-86400 || feed == nil) {
		return rssIcon{cached, site, checked}
	}
	u, err := url.Parse(site)
	if err != nil {
		return rssIcon{URL: cached, Site: cachedSite, Checked: checked}
	}
	fallback := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/favicon.ico"}).String()
	icon, err := p.findSiteIcon(ctx, site, agent)
	if err != nil {
		if cached != "" && cachedSite == site {
			fallback = cached
		}
		// Retry a failed optional lookup in an hour, without delaying article polling.
		return rssIcon{fallback, site, now - 23*3600}
	}
	if icon == "" {
		icon = fallback
	}
	return rssIcon{icon, site, now}
}

func (p *Poller) fetchSiteIcon(parent context.Context, site, agent string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, min(p.config.timeout(), 2*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", site, nil)
	if err != nil {
		return "", err
	}
	if err = validFeedURL(site); err != nil {
		return "", err
	}
	if err = p.reserveRedirect(req); err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "text/html, application/xhtml+xml")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	base := req.URL
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	p.observeHostResponse(ctx, hostGroup(base), resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("website icon lookup returned an unsuccessful HTTP status")
	}
	limit := min(p.config.Poll.MaxBytes, int64(256<<10))
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	if err = checkHTMLDocument(data); err != nil {
		return "", err
	}
	icon := htmlSiteIcon(data, base.String())
	if truncated && icon == "" {
		return "", errors.New("website icon discovery exceeds size limit")
	}
	return icon, nil
}

func htmlSiteIcon(data []byte, base string) string {
	z := xhtml.NewTokenizer(bytes.NewReader(data))
	var icons, touch []string
	baseSet := false
	for {
		kind := z.Next()
		if kind == xhtml.ErrorToken {
			break
		}
		if kind == xhtml.EndTagToken {
			name, _ := z.TagName()
			if string(name) == "head" {
				break
			}
		}
		if kind != xhtml.StartTagToken && kind != xhtml.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		attrs := map[string]string{}
		for _, attr := range token.Attr {
			attrs[strings.ToLower(attr.Key)] = attr.Val
		}
		if token.Data == "base" && !baseSet {
			if resolved := resolvedIcon(base, attrs["href"]); resolved != "" {
				base = resolved
				baseSet = true
			}
		}
		if token.Data != "link" {
			continue
		}
		for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
			switch rel {
			case "icon":
				icons = append(icons, attrs["href"])
			case "apple-touch-icon", "apple-touch-icon-precomposed":
				touch = append(touch, attrs["href"])
			}
		}
	}
	for _, candidate := range append(icons, touch...) {
		if resolved := resolvedIcon(base, candidate); resolved != "" {
			return resolved
		}
	}
	return ""
}
