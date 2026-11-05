package roi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestStarredDashboardPaginationAndDisabledFeeds(t *testing.T) {
	a := testApp(t)
	feed := addFeed(t, a, "https://example.org/starred")
	mustExec(t, a, "UPDATE feeds SET title='Saved feed' WHERE id=?", feed)
	for i := 0; i < 51; i++ {
		save(t, a, feed, &gofeed.Item{GUID: fmt.Sprint(i), Title: fmt.Sprintf("Saved %02d", i), Link: fmt.Sprintf("https://example.org/article/%d", i)})
	}
	mustExec(t, a, "UPDATE entries SET starred=1,published=1700000000")
	mustExec(t, a, "UPDATE feeds SET active=0 WHERE id=?", feed)
	cookie, _ := adminSession(t, a)
	for _, tc := range []struct {
		query      string
		page, rows int
		first      string
	}{
		{"", 1, 50, "Saved 50"}, {"?starred_page=2&disabled=1", 2, 1, "Saved 00"}, {"?starred_page=999", 2, 1, "Saved 00"}, {"?starred_page=bad", 1, 50, "Saved 50"},
	} {
		w := adminRequest(a, cookie, "GET", "/starred"+tc.query, nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "51 starred articles") {
			t.Fatal(w.Code, body)
		}
		table := strings.Split(strings.Split(body, `<table aria-label="Starred articles">`)[1], "</table>")[0]
		if strings.Count(table, "<tr>") != tc.rows+1 || !strings.Contains(table, tc.first) || !strings.Contains(table, "Saved feed") || !strings.Contains(table, ">Disabled</span>") || !strings.Contains(table, `data-unix="1700000000"`) {
			t.Fatal(table)
		}
		if !strings.Contains(body, fmt.Sprintf("Page %d · 51 starred articles", tc.page)) {
			t.Fatal("wrong page")
		}
	}
	w := asyncAdmin(a, cookie, "GET", "/starred?fragment=1&starred_page=2", nil)
	if !strings.Contains(w.Body.String(), `id="starred-articles"`) {
		t.Fatal(w.Body.String())
	}
	mustExec(t, a, "UPDATE entries SET starred=0")
	w = adminRequest(a, cookie, "GET", "/starred?starred_page=2", nil)
	if !strings.Contains(w.Body.String(), "No starred articles.") || !strings.Contains(w.Body.String(), "0 starred articles") {
		t.Fatal(w.Body.String())
	}
}

func TestStarredStateAcrossDevicesAndIncrementalSync(t *testing.T) {
	a := testApp(t)
	feed := addFeed(t, a, "https://example.org/rss")
	save(t, a, feed, &gofeed.Item{GUID: "stable", Title: "Stable article", Link: "https://example.org/article"})
	mustExec(t, a, "UPDATE entries SET updated=10,published=12345")
	one, two := approvedToken(t, a), approvedToken(t, a)
	var id int64
	if err := a.store.DB.QueryRow("SELECT id FROM entries").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"a", "r"} {
		mustExec(t, a, "UPDATE entries SET updated=10")
		w := apiReq(a, one, "POST", "/reader/api/0/edit-tag", url.Values{"i": {entryID(id)}, op: {statePrefix + "starred"}})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		w = apiReq(a, two, "GET", "/reader/api/0/stream/contents/reading-list", url.Values{"ot": {"11"}})
		var result struct {
			Items []struct {
				ID                 string
				Published, Updated int64
				Categories         []string
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != 1 || result.Items[0].ID != entryID(id) || result.Items[0].Published != 12345 || result.Items[0].Updated <= 10 {
			t.Fatal(w.Body.String())
		}
		starred := false
		for _, category := range result.Items[0].Categories {
			if strings.HasSuffix(category, "/starred") {
				starred = true
			}
		}
		if starred != (op == "a") {
			t.Fatal(w.Body.String())
		}
		w = apiReq(a, two, "GET", "/reader/api/0/stream/items/ids", url.Values{"s": {statePrefix + "starred"}})
		var refs struct {
			Items []any `json:"itemRefs"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &refs); err != nil {
			t.Fatal(err)
		}
		want := 0
		if op == "a" {
			want = 1
		}
		if len(refs.Items) != want {
			t.Fatal(w.Body.String())
		}
		var count int
		if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&count); err != nil || count != 1 {
			t.Fatal(count, err)
		}
	}
	// Replayed state does not falsely advertise another change.
	mustExec(t, a, "UPDATE entries SET updated=10")
	if w := apiReq(a, one, "POST", "/reader/api/0/edit-tag", url.Values{"i": {entryID(id)}, "r": {statePrefix + "starred"}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var updated int64
	if err := a.store.DB.QueryRow("SELECT updated FROM entries WHERE id=?", id).Scan(&updated); err != nil || updated != 10 {
		t.Fatal(updated, err)
	}
}
