package roi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImportPreviewNoChangesAndSharedValidation(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/known")
	if err := a.store.SetEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	raw := `format: roi
version: 1
sources:
 - type: rss
   url: http://example.org/known
   folders: [Other]
 - type: rss
   url: https://example.org/new
 - type: rss
   url: http://example.org/new
 - type: git
   repository: https://github.com/team/repo
   service: github
 - type: rss
   url: file:///invalid
 - type: rss
   url: https://example.org/invalid-folder
   folders: [GIT_Releases]
`
	preview, err := a.store.PreviewImport(strings.NewReader(raw), "roi")
	if err != nil || preview.Total != 6 || preview.Add != 2 || preview.Skipped != 2 || preview.Invalid != 2 {
		t.Fatal(preview, err)
	}
	if preview.Items[0].Status != "skip" || preview.Items[1].Status != "add" || preview.Items[2].Status != "skip" || preview.Items[4].Error == "" {
		t.Fatal(preview)
	}
	if err := a.store.ImportROI(strings.NewReader(raw)); err == nil {
		t.Fatal("invalid plan committed")
	}
	feeds, _ := a.store.Feeds()
	if len(feeds) != 1 || feeds[0].Active || len(feeds[0].Folders) != 0 {
		t.Fatal("preview changed data", feeds)
	}
	opml := `<opml><body><outline text="Folder"><outline xmlUrl="http://example.org/known"/><outline text="New" xmlUrl="https://example.org/new"/></outline></body></opml>`
	preview, err = a.store.PreviewImport(strings.NewReader(opml), "opml")
	if err != nil || preview.Add != 1 || preview.Skipped != 1 || preview.Invalid != 0 {
		t.Fatal(preview, err)
	}
	if err := a.store.Import(strings.NewReader(opml)); err != nil {
		t.Fatal(err)
	}
	preview, err = a.store.PreviewImport(strings.NewReader(opml), "opml")
	if err != nil || preview.Add != 0 || preview.Skipped != 2 {
		t.Fatal(preview, err)
	}
	for _, bad := range []struct{ data, format string }{{"broken", "roi"}, {"", "other"}, {"<opml/><opml/>", "opml"}, {"<opml/>", "roi"}, {"<opml>", "opml"}} {
		if _, err := a.store.PreviewImport(strings.NewReader(bad.data), bad.format); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestImportPreviewAdminAuthenticationAndCSRF(t *testing.T) {
	a := testApp(t)
	cookie, csrf := adminSession(t, a)
	for _, tc := range []struct {
		auth   bool
		csrf   string
		status int
	}{{false, csrf, 401}, {true, "wrong", 403}, {true, csrf, 200}} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("csrf", tc.csrf)
		_ = writer.WriteField("format", "roi")
		part, _ := writer.CreateFormFile("file", "subscriptions.roi")
		_, _ = io.WriteString(part, "format: roi\nversion: 1\nsources:\n - type: rss\n   url: https://example.org/rss\n")
		_ = writer.Close()
		r := httptest.NewRequest("POST", "/preview-import", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		r.Header.Set("Accept", "application/json")
		if tc.auth {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		a.AdminHandler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var preview ImportPreview
			if err := json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.Add != 1 {
				t.Fatal(preview, err)
			}
		}
	}
	feeds, _ := a.store.Feeds()
	if len(feeds) != 0 {
		t.Fatal("preview imported subscriptions")
	}
}
