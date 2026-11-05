package roi

import (
	"strings"
	"testing"
)

func TestAdminPagesAndFragmentIsolation(t *testing.T) {
	a := testApp(t)
	cookie, _ := adminSession(t, a)
	sections := map[string]string{"feeds": "feed-list", "starred": "starred-articles", "devices": "reader-sessions", "security": "security-events", "maintenance": "storage-footer", "import-export": "import-export"}
	for page, section := range sections {
		for _, suffix := range []string{"", "?fragment=1"} {
			w := adminRequest(a, cookie, "GET", "/"+page+suffix, nil)
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, `id="page-content" data-page="`+page+`"`) || !strings.Contains(body, `id="`+section+`"`) {
				t.Fatal(page, suffix, w.Code, body)
			}
			if page == "feeds" {
				summary := strings.Split(strings.Split(body, `<div id="summary"`)[1], "</div></div>")[0]
				if strings.Count(summary, `class="card"`) != 4 {
					t.Fatal("expected four feed statistics cards")
				}
				previous := -1
				for _, label := range []string{"Subscriptions", "Entries", "Unread", "Starred"} {
					index := strings.Index(summary, "<span>"+label+"</span>")
					if index <= previous {
						t.Fatal("incorrect feed statistics order", label)
					}
					previous = index
				}
			}
			for other, id := range sections {
				if other != page && strings.Contains(body, `id="`+id+`"`) {
					t.Fatalf("%s includes %s content", page, other)
				}
			}
			if suffix == "" {
				if !strings.Contains(body, `href="/`+page+`" data-nav aria-current="page"`) || strings.Count(body, `aria-current="page"`) != 1 {
					t.Fatal("wrong active navigation", page)
				}
			} else if strings.Contains(body, "<script") || strings.Contains(body, "<header") || strings.Contains(body, "Main navigation") {
				t.Fatal("fragment includes page shell")
			}
		}
	}
	if w := adminRequest(a, cookie, "GET", "/unknown", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	// Device requests must not depend on feed history or storage aggregation.
	mustExec(t, a, "DROP TABLE fetches")
	if w := adminRequest(a, cookie, "GET", "/devices?fragment=1", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := adminRequest(a, cookie, "GET", "/starred?fragment=1", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
