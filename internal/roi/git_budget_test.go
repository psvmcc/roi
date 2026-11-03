package roi

import (
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitDownloadBudgetReportsCumulativeDecompressedBytes(t *testing.T) {
	a := testApp(t)
	a.poller.config.Poll.MaxBytes = 1024
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		size := 512
		if r.URL.Path == "/second" {
			size = 2048
		}
		w.Header().Set("Content-Encoding", "gzip")
		out := gzip.NewWriter(w)
		_, _ = out.Write([]byte(strings.Repeat("x", size)))
		_ = out.Close()
	}))
	defer server.Close()
	a.poller.client = server.Client()
	budget := gitBudget{Limit: 1024}
	if body, err := a.poller.gitGet(t.Context(), server.URL+"/first", "private-token", &budget); err != nil || len(body) != 512 || budget.Received != 512 {
		t.Fatal(len(body), budget, err)
	}
	raw := server.URL + "/second?page=2"
	_, err := a.poller.gitGet(t.Context(), raw, "private-token", &budget)
	var size *gitSizeError
	if !errors.As(err, &size) || size.Limit != 1024 || size.Received != 1025 || size.ConfiguredLimit != 1024 || size.URL != raw {
		t.Fatal(size, err)
	}
	for _, part := range []string{raw, "received at least 1025 bytes", "operation budget 1024 bytes", "poll.max_bytes=1024", "after decompression", "pages and commit lookups share"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatal("missing size-limit context", part, err)
		}
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatal("error leaked authorization token")
	}
	if category := syncErrorCategory(err, 200, "parse"); category != "Size limit" {
		t.Fatal(category)
	}
	_, err = a.poller.gitGet(t.Context(), server.URL+"/third", "", &budget)
	if err == nil || requests.Load() != 2 {
		t.Fatal("exhausted budget issued another request", requests.Load(), err)
	}
}

func TestGitDownloadBudgetExactLimitAndDiscoveryCap(t *testing.T) {
	a := testApp(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer server.Close()
	budget := gitBudget{Limit: 1024}
	if body, err := a.poller.gitGet(t.Context(), server.URL, "", &budget); err != nil || len(body) != 1024 {
		t.Fatal(len(body), err)
	}
	_, err := a.poller.gitGet(t.Context(), server.URL+"/next", "", &budget)
	var size *gitSizeError
	if !errors.As(err, &size) || size.Received != 1024 || requests.Load() != 1 {
		t.Fatal(size, requests.Load(), err)
	}
	// Discovery intentionally uses smaller local caps than poll.max_bytes.
	// Its error must report both values instead of claiming the global cap failed.
	budget = gitBudget{Limit: 64}
	_, err = a.poller.gitGet(t.Context(), server.URL, "", &budget)
	if !errors.As(err, &size) || size.Limit != 64 || size.Received != 65 || size.ConfiguredLimit != a.poller.config.Poll.MaxBytes {
		t.Fatal(size, err)
	}
}
