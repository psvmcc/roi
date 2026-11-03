package roi

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

func TestReaderUsesOriginalPublicationDatesAndPreservesCrawlTime(t *testing.T) {
	a := testApp(t)
	feed := addFeed(t, a, "https://example.org/rss")
	published := time.Date(2009, 10, 14, 16, 12, 40, 0, time.FixedZone("source", 3*3600))
	received := time.Date(2026, 11, 1, 14, 0, 0, 123000000, time.UTC)
	if _, e := a.poller.saveEntries(context.Background(), feed, &gofeed.Feed{Items: []*gofeed.Item{{GUID: "old", Title: "Old post", PublishedParsed: &published}}}, received); e != nil {
		t.Fatal(e)
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", nil)
	var ids struct {
		Refs []struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestampUsec"`
		} `json:"itemRefs"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &ids); e != nil || len(ids.Refs) != 1 || ids.Refs[0].Timestamp != strconv.FormatInt(published.UnixMicro(), 10) {
		t.Fatal(w.Body.String(), e)
	}
	for _, route := range []string{"stream/items/contents", "stream/contents/reading-list"} {
		w := apiReq(a, token, "POST", "/reader/api/0/"+route, url.Values{"i": {ids.Refs[0].ID}})
		var result struct {
			Items []struct {
				Published int64  `json:"published"`
				Timestamp string `json:"timestampUsec"`
				Crawl     string `json:"crawlTimeMsec"`
			} `json:"items"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil || w.Code != 200 || len(result.Items) != 1 {
			t.Fatal(w.Code, w.Body.String(), e)
		}
		x := result.Items[0]
		if x.Published != published.Unix() || x.Timestamp != strconv.FormatInt(published.UnixMicro(), 10) || x.Crawl != strconv.FormatInt(received.UnixMilli(), 10) {
			t.Fatal("source and receipt dates mixed", x)
		}
	}
}
func TestDateOnlyCorrectionsRefreshMetadataAndPublicationStatistics(t *testing.T) {
	a := testApp(t)
	feed := addFeed(t, a, "https://example.org/rss")
	original := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	corrected := original.AddDate(-1, 0, 0)
	received := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	item := &gofeed.Item{GUID: "same", Title: "same", Content: "same", PublishedParsed: &original}
	f := &gofeed.Feed{Items: []*gofeed.Item{item}}
	if _, e := a.poller.saveEntries(context.Background(), feed, f, received); e != nil {
		t.Fatal(e)
	}
	mustExec(t, a, "UPDATE entries SET is_read=1,starred=1")
	item.PublishedParsed = &corrected
	if n, e := a.poller.saveEntries(context.Background(), feed, f, received.Add(time.Hour)); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	var id, pub, first, updated int64
	var read, star bool
	if e := a.store.DB.QueryRow("SELECT id,published,received,updated,is_read,starred FROM entries").Scan(&id, &pub, &first, &updated, &read, &star); e != nil {
		t.Fatal(e)
	}
	if id != 1 || pub != corrected.Unix() || first != received.UnixMicro() || updated != received.Add(time.Hour).Unix() || !read || !star {
		t.Fatal(id, pub, first, updated, read, star)
	}
	fs, e := a.store.Feeds()
	if e != nil || fs[0].Published != corrected.Unix() {
		t.Fatal("stale feed statistic", fs, e)
	}
	mustExec(t, a, "UPDATE feeds SET last_published=9999999999")
	fs, e = a.store.Feeds()
	if e != nil || fs[0].Published != corrected.Unix() {
		t.Fatal("cached value leaked into admin", fs, e)
	}
	token := approvedToken(t, a)
	w := apiReq(a, token, "GET", "/reader/api/0/stream/items/ids", url.Values{"ot": {strconv.FormatInt(received.Unix()+1, 10)}})
	if !strings.Contains(w.Body.String(), `"id":"1"`) {
		t.Fatal("date correction missing from incremental sync", w.Body.String())
	}
}
func TestMissingDatesKeepFirstReceiptAndSourceUpdatedFallback(t *testing.T) {
	a := testApp(t)
	feed := addFeed(t, a, "https://example.org/rss")
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	updated := at.AddDate(-2, 0, 0)
	zero := time.Time{}
	item := &gofeed.Item{GUID: "no-date", Title: "first", PublishedParsed: &zero}
	f := &gofeed.Feed{Items: []*gofeed.Item{item}}
	if _, e := a.poller.saveEntries(context.Background(), feed, f, at); e != nil {
		t.Fatal(e)
	}
	item.Title = "changed"
	if _, e := a.poller.saveEntries(context.Background(), feed, f, at.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	var pub int64
	if e := a.store.DB.QueryRow("SELECT published FROM entries").Scan(&pub); e != nil || pub != at.Unix() {
		t.Fatal("dateless entry moved to sync date", pub, e)
	}
	item.UpdatedParsed = &updated
	if _, e := a.poller.saveEntries(context.Background(), feed, f, at.Add(2*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if e := a.store.DB.QueryRow("SELECT published FROM entries").Scan(&pub); e != nil || pub != updated.Unix() {
		t.Fatal("source updated fallback ignored", pub, e)
	}
}
