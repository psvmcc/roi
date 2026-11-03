package roi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
)

func (p *Poller) detectGit(ctx context.Context, s GitSource) (GitSource, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.timeout())
	defer cancel()
	if err := p.resources.acquire(ctx, 2*p.config.Poll.MaxBytes); err != nil {
		return s, err
	}
	defer p.resources.release(2 * p.config.Poll.MaxBytes)
	u, _ := url.Parse(s.URL)
	host := hostGroup(u)
	if err := p.waitHost(ctx, host); err != nil {
		return s, err
	}
	defer p.releaseHost(host)
	u, _ = url.Parse(s.URL)
	if s.Provider == "auto" {
		switch strings.ToLower(u.Hostname()) {
		case "github.com":
			s.Provider = "github"
		case "gitlab.com":
			s.Provider = "gitlab"
		case "codeberg.org":
			s.Provider = "forgejo"
		case "gitea.com":
			s.Provider = "gitea"
		}
	}
	budget := gitBudget{Limit: min(p.config.Poll.MaxBytes, int64(1<<20))}
	discoveryURL := s.origin() + "/"
	if s.Provider == "gitweb" {
		discoveryURL = s.URL
	}
	if page, err := p.gitGet(ctx, discoveryURL, "", &budget); err == nil {
		if err = checkHTMLDocument(page); err != nil {
			return s, err
		}
		doc, err := xhtml.Parse(bytes.NewReader(page))
		if err == nil {
			var walk func(*xhtml.Node)
			walk = func(n *xhtml.Node) {
				if n.Type == xhtml.ElementNode {
					attrs := map[string]string{}
					for _, a := range n.Attr {
						attrs[strings.ToLower(a.Key)] = a.Val
					}
					if n.Data == "meta" && (strings.EqualFold(attrs["name"], "application-name") || strings.EqualFold(attrs["name"], "generator")) && s.Provider == "auto" {
						value := strings.ToLower(attrs["content"])
						for _, provider := range []string{"forgejo", "gitea", "gitlab", "github", "gitweb"} {
							if strings.Contains(value, provider) {
								s.Provider = provider
								break
							}
						}
					}
					if n.Data == "link" {
						for _, rel := range strings.Fields(strings.ToLower(attrs["rel"])) {
							if rel == "icon" {
								if icon, err := url.Parse(attrs["href"]); err == nil {
									base, _ := url.Parse(discoveryURL)
									icon = base.ResolveReference(icon)
									if icon.Scheme == "https" || icon.Scheme == "http" {
										s.Icon = icon.String()
									}
								}
							}
						}
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(doc)
		}
	}
	if s.Provider == "auto" {
		for _, probe := range []struct{ provider, path string }{{"forgejo", "/api/forgejo/v1/version"}, {"gitea", "/api/v1/version"}, {"gitlab", "/api/v4/version"}} {
			budget := gitBudget{Limit: 64 << 10}
			b, err := p.gitGet(ctx, s.origin()+probe.path, p.gitToken(s), &budget)
			if err != nil {
				continue
			}
			var v struct{ Version string }
			if json.Unmarshal(b, &v) == nil && v.Version != "" {
				s.Provider = probe.provider
				break
			}
		}
	}
	if s.Provider == "auto" {
		return s, clientError("cannot detect Git service; select the service explicitly for this host")
	}
	validated, err := parseGitSource(s.URL, s.Provider, s.Mode, s.Preview)
	if err != nil {
		return s, err
	}
	validated.Icon = s.Icon
	validated.TagFilter = s.TagFilter
	if validated.Provider == "gitweb" {
		budget := gitBudget{Limit: p.config.Poll.MaxBytes}
		b, err := p.gitGet(ctx, validated.gitwebURL("tags", ""), "", &budget)
		if err != nil {
			return s, err
		}
		if _, err = parseGitwebTags(b, validated); err != nil {
			return s, err
		}
		return validated, nil
	}
	// Verify the repository API before storing a subscription; never save an HTML error page as a feed.
	budget = gitBudget{Limit: p.config.Poll.MaxBytes}
	raw := validated.endpoint() + "?page=1&per_page=1&limit=1"
	b, err := p.gitGet(ctx, raw, p.gitToken(validated), &budget)
	if err != nil {
		return s, err
	}
	if _, err = decodeGitList(b); err != nil {
		return s, clientError("Git repository API did not return a list")
	}
	return validated, nil
}
