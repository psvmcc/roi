package roi

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckpointTruncatesAndPreservesData(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	mustExec(t, a, "UPDATE feeds SET title=? WHERE id=?", strings.Repeat("x", 100000), id)
	path := a.config.Database + "-wal"
	before, err := os.Stat(path)
	if err != nil || before.Size() == 0 {
		t.Fatal("expected WAL writes", err)
	}
	for _, idle := range []bool{false, true} {
		ok, err := a.store.Checkpoint(context.Background(), idle)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() != 0 {
		t.Fatal("WAL not truncated", err)
	}
	var title string
	if err := a.store.DB.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil || len(title) != 100000 {
		t.Fatal("lost data", err)
	}
	var timeout int
	if err := a.store.DB.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatal("timeout not restored", timeout, err)
	}
}

func TestCheckpointExternalReaderDefersWithoutBlocking(t *testing.T) {
	a := testApp(t)
	id := addFeed(t, a, "https://example.org/rss")
	reader, err := sql.Open("sqlite", a.config.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var title string
	if err := tx.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil {
		t.Fatal(err)
	}
	mustExec(t, a, "UPDATE feeds SET title='Changed' WHERE id=?", id)
	start := time.Now()
	for _, idle := range []bool{true, false} {
		ok, err := a.store.Checkpoint(context.Background(), idle)
		if err != nil || ok {
			t.Fatal("reader should defer checkpoint", ok, err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("checkpoint waited for external reader")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.store.Checkpoint(context.Background(), true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := a.store.DB.QueryRow("SELECT title FROM feeds WHERE id=?", id).Scan(&title); err != nil || title != "Changed" {
		t.Fatal("lost committed write", title, err)
	}
}

func TestCheckpointCancellationAndClosedStore(t *testing.T) {
	a := testApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.store.Checkpoint(ctx, true); err == nil {
		t.Fatal("cancelled checkpoint succeeded")
	}
	a.store.DB.Close()
	if _, err := a.store.Checkpoint(context.Background(), true); err == nil {
		t.Fatal("closed checkpoint succeeded")
	}
	a.poller.checkpoint(context.Background(), true)
	a.poller.checkpoint(ctx, true)
}

func TestPollerCheckpointsFinishedBatch(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><title>Test</title><item><guid>one</guid><title>One</title></item></channel></rss>`))
	}))
	defer source.Close()
	a := testApp(t)
	addFeed(t, a, source.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.poller.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := a.store.DB.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(a.config.Database + "-wal")
		if count == 1 && err == nil && info.Size() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("completed sync batch did not checkpoint WAL")
}
