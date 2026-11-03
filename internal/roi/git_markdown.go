package roi

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	xhtml "golang.org/x/net/html"
)

var gitMarkdownRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM))

func gitMarkdown(body, releaseURL string) (string, error) {
	body = strings.ToValidUTF8(body, "�")
	truncated := len(body) > 100000
	if truncated {
		body = body[:100000]
		for !utf8.ValidString(body) && len(body) > 0 {
			body = body[:len(body)-1]
		}
	}
	if strings.TrimSpace(body) == "" {
		return `<p><a href="` + html.EscapeString(releaseURL) + `">Open release notes</a></p>`, nil
	}
	var b bytes.Buffer
	if err := gitMarkdownRenderer.Convert([]byte(body), &b); err != nil {
		return "", err
	}
	// Resolve relative Markdown links and forbid non-web/non-mailto URLs. Raw HTML
	// and unsafe links are already disabled by goldmark's default renderer.
	doc, err := xhtml.Parse(strings.NewReader(b.String()))
	if err != nil {
		return "", err
	}
	base, _ := url.Parse(releaseURL)
	var content *xhtml.Node
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "body" {
			content = n
		}
		for i, a := range n.Attr {
			if a.Key == "href" || a.Key == "src" {
				u, err := url.Parse(a.Val)
				if err != nil {
					n.Attr[i].Val = ""
					continue
				}
				u = base.ResolveReference(u)
				if u.Scheme != "http" && u.Scheme != "https" && (a.Key != "href" || u.Scheme != "mailto") {
					n.Attr[i].Val = ""
				} else {
					n.Attr[i].Val = u.String()
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	b.Reset()
	if content != nil {
		for n := content.FirstChild; n != nil; n = n.NextSibling {
			if err := xhtml.Render(&b, n); err != nil {
				return "", err
			}
		}
	}
	if truncated {
		fmt.Fprintf(&b, `<p>Description truncated. <a href="%s">Open complete release notes</a></p>`, html.EscapeString(releaseURL))
	}
	return b.String(), nil
}
