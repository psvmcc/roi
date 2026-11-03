package roi

import (
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
)

// Translate the small HTML vocabulary used by Gitweb to Markdown. Rendering
// still goes through Goldmark's safe renderer and URL checks, never raw HTML.
func gitwebMessage(n *xhtml.Node) string {
	var render func(*xhtml.Node) string
	render = func(node *xhtml.Node) string {
		if node.Type == xhtml.TextNode {
			text := strings.ReplaceAll(node.Data, "\u00a0", " ")
			if node.PrevSibling != nil && node.PrevSibling.Data == "br" {
				text = strings.TrimPrefix(text, "\n")
			}
			return text
		}
		if node.Type != xhtml.ElementNode {
			return ""
		}
		if node.Data == "script" || node.Data == "style" {
			return ""
		}
		var b strings.Builder
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			b.WriteString(render(c))
		}
		text := b.String()
		switch node.Data {
		case "br":
			return "\n"
		case "a":
			u, err := url.Parse(htmlAttr(node, "href"))
			if err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") {
				return text
			}
			label := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(text)
			return "[" + label + "](<" + u.String() + ">)"
		case "pre":
			fence := "```"
			for strings.Contains(text, fence) {
				fence += "`"
			}
			return "\n\n" + fence + "\n" + htmlText(node) + "\n" + fence + "\n\n"
		case "code":
			if node.Parent != nil && node.Parent.Data == "pre" {
				return text
			}
			fence := "`"
			for strings.Contains(text, fence) {
				fence += "`"
			}
			return fence + " " + text + " " + fence
		case "p":
			return "\n\n" + text + "\n\n"
		case "li":
			return "\n- " + strings.TrimSpace(text) + "\n"
		case "strong", "b":
			return "**" + text + "**"
		case "em", "i":
			return "*" + text + "*"
		}
		return text
	}
	return render(n)
}
