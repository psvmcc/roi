package roi

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func brokenGzip(t *testing.T, body, kind string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := gzip.NewWriter(&buffer)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	switch kind {
	case "header":
		return []byte("this is not a gzip stream")
	case "flate":
		return append(append([]byte{}, data[:10]...), 7) // Invalid DEFLATE block type.
	case "truncated":
		return data[:len(data)-4]
	default:
		data[len(data)-8] ^= 255 // Corrupt the checksum after a complete body.
		return data
	}
}

func TestGitCorruptCompressionRetriesIdentityAndCachesOnlySuccess(t *testing.T) {
	for _, kind := range []string{"header", "flate", "truncated", "checksum"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			body := "<html>Complete Gitweb page</html>"
			broken := brokenGzip(t, body, kind)
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing Git authorization")
				}
				if n == 3 {
					if r.Header.Get("If-None-Match") != `"complete"` {
						t.Error("wrong cached validator")
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
				if n == 1 {
					if r.Header.Get("Accept-Encoding") != "gzip" {
						t.Error("first request should use normal compression")
					}
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("ETag", `"broken"`)
					_, _ = w.Write(broken)
					return
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("retry must disable compression")
				}
				w.Header().Set("ETag", `"complete"`)
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a.poller.client = server.Client()
			budget := gitBudget{Limit: 1024}
			next := false
			got, err := a.poller.gitGet(t.Context(), server.URL, "test-token", &budget, &next)
			if err != nil || string(got) != body || !next || calls.Load() != 2 {
				t.Fatal(string(got), next, calls.Load(), err)
			}
			if kind == "checksum" && budget.Received != int64(2*len(body)) {
				t.Fatal("partial failed response was not charged", budget)
			}
			got, err = a.poller.gitGet(t.Context(), server.URL, "test-token", &budget)
			if err != nil || string(got) != body || calls.Load() != 3 {
				t.Fatal("fallback body was not cached", string(got), err)
			}
			if a.metrics.GetOrCreateCounter("roi_git_identity_retries_total").Get() != 1 {
				t.Fatal("retry counter")
			}
		})
	}
}

func TestGitCompressionRetrySharesBudgetAndStopsAfterOneAttempt(t *testing.T) {
	for _, scenario := range []string{"budget", "exhausted", "still-compressed", "http-error"} {
		t.Run(scenario, func(t *testing.T) {
			a := testApp(t)
			body := strings.Repeat("x", 32)
			broken := brokenGzip(t, body, "checksum")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 || scenario == "still-compressed" {
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(broken)
					return
				}
				if scenario == "http-error" {
					w.WriteHeader(503)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			budget := gitBudget{Limit: 48}
			if scenario == "exhausted" {
				budget.Limit = 16
			}
			_, err := a.poller.gitGet(t.Context(), server.URL, "", &budget)
			wantCalls := int32(2)
			if scenario == "exhausted" {
				wantCalls = 1
			}
			if err == nil || calls.Load() != wantCalls {
				t.Fatal(calls.Load(), err)
			}
			if scenario == "budget" || scenario == "exhausted" {
				var size *gitSizeError
				if !errors.As(err, &size) || budget.Received != budget.Limit+1 {
					t.Fatal(budget, err)
				}
			}
			if scenario == "still-compressed" && (syncErrorCategory(err, 0, "parse") != "Network" || !strings.Contains(err.Error(), server.URL)) {
				t.Fatal("misleading compression error", err)
			}
			var count int
			if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM git_http_cache").Scan(&count); err != nil || count != 0 {
				t.Fatal("failed response entered cache", count, err)
			}
		})
	}
}

type compressionReadFailure struct {
	cancel context.CancelFunc
	err    error
}

func (r compressionReadFailure) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return 0, r.err
}

func TestGitCompressionRetryCancellationAndSharedDeadline(t *testing.T) {
	for _, scenario := range []string{"canceled", "deadline", "ordinary-read-error"} {
		t.Run(scenario, func(t *testing.T) {
			a := testApp(t)
			a.poller.config.Poll.Timeout = "50ms"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			var deadline time.Time
			a.poller.client.Transport = gitRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				at, _ := r.Context().Deadline()
				if calls == 2 {
					if at != deadline || r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("retry reset deadline or compression")
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				deadline = at
				reader := compressionReadFailure{err: flate.CorruptInputError(1)}
				if scenario == "canceled" {
					reader.cancel = cancel
				}
				if scenario == "ordinary-read-error" {
					reader.err = errors.New("ordinary read failure")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(reader), Uncompressed: true}, nil
			})
			budget := gitBudget{Limit: 1024}
			_, err := a.poller.gitGet(ctx, "https://example.org/?p=repo.git", "", &budget)
			if scenario == "deadline" {
				if !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
					t.Fatal(calls, err)
				}
			} else if err == nil || calls != 1 || scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(calls, err)
			}
		})
	}
}
