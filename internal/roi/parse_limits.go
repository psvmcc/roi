package roi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sync"

	"go.yaml.in/yaml/v3"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

type parseGate struct {
	mu              sync.Mutex
	active, limit   int
	bytes, capacity int64
	changed         chan struct{}
}

func newParseGate(s SecurityConfig) *parseGate {
	return &parseGate{limit: s.ParseConcurrency, capacity: s.ParseMemoryBytes, changed: make(chan struct{})}
}
func (g *parseGate) acquire(ctx context.Context, n int64) error {
	if n > g.capacity {
		return fmt.Errorf("document exceeds parser memory admission budget")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		if g.active < g.limit && g.bytes+n <= g.capacity {
			g.active++
			g.bytes += n
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (g *parseGate) release(n int64) {
	g.mu.Lock()
	g.active--
	g.bytes -= n
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

const maxParseTokens = 200000

func checkFeedDocument(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return checkJSONDocument(data)
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	// Match gofeed's tolerant tokenization for real-world RSS/Atom feeds.
	d.Strict = false
	d.CharsetReader = charset.NewReaderLabel
	depth, items := 0, 0
	for count := 0; ; count++ {
		if count >= maxParseTokens {
			return fmt.Errorf("XML document exceeds parser token limit")
		}
		token, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case xml.StartElement:
			depth++
			if depth > 128 || len(token.Attr) > 64 {
				return fmt.Errorf("XML document exceeds parser depth/attribute limit")
			}
			if token.Name.Local == "item" || token.Name.Local == "entry" {
				items++
				if items > 10000 {
					return fmt.Errorf("feed contains more than 10000 articles")
				}
			}
		case xml.EndElement:
			depth--
		}
	}
}
func checkJSONDocument(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	for count := 0; ; count++ {
		if count >= maxParseTokens {
			return fmt.Errorf("JSON document exceeds parser token limit")
		}
		token, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
			if depth > 128 {
				return fmt.Errorf("JSON document exceeds parser depth limit")
			}
		}
	}
}
func checkHTMLDocument(data []byte) error {
	z := xhtml.NewTokenizer(bytes.NewReader(data))
	z.SetMaxBuf(4 << 20)
	for count := 0; ; count++ {
		if count >= maxParseTokens {
			return fmt.Errorf("HTML document exceeds parser token limit")
		}
		if z.Next() == xhtml.ErrorToken {
			if z.Err() == io.EOF {
				return nil
			}
			return z.Err()
		}
	}
}

// A lexical width bound runs before yaml.v3 allocates its syntax tree. Tree
// depth, node count and aliases are checked before decoding application types.
func checkYAMLDocument(data []byte) error {
	width := 0
	for _, c := range data {
		if c == '\n' || c == ',' || c == '[' || c == '{' || c == ']' || c == '}' {
			width++
			if width > 200000 {
				return fmt.Errorf("YAML document exceeds parser width limit")
			}
		}
	}
	var node yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&node); err != nil {
		return err
	}
	count := 0
	var walk func(*yaml.Node, int) error
	walk = func(n *yaml.Node, depth int) error {
		count++
		if count > 300000 || depth > 64 {
			return fmt.Errorf("YAML document exceeds parser node/depth limit")
		}
		if n.Kind == yaml.AliasNode {
			return fmt.Errorf("YAML aliases are not supported")
		}
		for _, child := range n.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&node, 0)
}
