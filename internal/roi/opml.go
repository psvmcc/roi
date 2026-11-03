package roi

import (
	"encoding/xml"
	"io"
)

type Outline struct {
	Text        string    `xml:"text,attr,omitempty"`
	Title       string    `xml:"title,attr,omitempty"`
	Type        string    `xml:"type,attr,omitempty"`
	URL         string    `xml:"xmlUrl,attr,omitempty"`
	Site        string    `xml:"htmlUrl,attr,omitempty"`
	GitIcon     string    `xml:"roiIcon,attr,omitempty"`
	GitProvider string    `xml:"roiProvider,attr,omitempty"`
	GitMode     string    `xml:"roiMode,attr,omitempty"`
	GitPreview  bool      `xml:"roiPreview,attr,omitempty"`
	Children    []Outline `xml:"outline"`
}

type OPML struct {
	XMLName  xml.Name  `xml:"opml"`
	Version  string    `xml:"version,attr"`
	Title    string    `xml:"head>title"`
	Outlines []Outline `xml:"body>outline"`
}

func (s *Store) Import(r io.Reader) error { return s.importFile(r, "opml") }

func (s *Store) Export(w io.Writer) error {
	fs, e := s.Feeds()
	if e != nil {
		return e
	}
	o := OPML{Version: "2.0", Title: "roi subscriptions"}
	groups := map[string]int{}
	for _, f := range fs {
		if !f.Active || f.GitProvider != "" {
			continue
		}
		v := Outline{Text: f.Title, Title: f.Title, Type: "rss", URL: f.URL, Site: f.Site}
		if len(f.Folders) == 0 {
			o.Outlines = append(o.Outlines, v)
		} else {
			for _, g := range f.Folders {
				i, ok := groups[g]
				if !ok {
					i = len(o.Outlines)
					groups[g] = i
					o.Outlines = append(o.Outlines, Outline{Text: g, Title: g})
				}
				o.Outlines[i].Children = append(o.Outlines[i].Children, v)
			}
		}
	}
	if _, e = io.WriteString(w, xml.Header); e != nil {
		return e
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(o)
}
