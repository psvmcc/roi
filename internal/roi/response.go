package roi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

var errResponseLimit = &statusError{http.StatusRequestEntityTooLarge, "article response exceeds security.max_response_bytes; request fewer items"}

type responseBudget struct {
	dst       io.Writer
	remaining int64
}

func (b *responseBudget) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		return 0, errResponseLimit
	}
	n, e := b.dst.Write(p)
	b.remaining -= int64(n)
	return n, e
}

// Encode one article at a time into a private bounded spool, then release SQLite
// before streaming to the client. No invalid/over-budget batch produces a 200
// or a silently truncated item set. Both memory and temporary storage are bounded.
func (a *App) articleResponse(w http.ResponseWriter, produce func(io.Writer) error) error {
	f, e := os.CreateTemp("", "roi-response-*")
	if e != nil {
		return e
	}
	// Unlink immediately where supported: a crash cannot leave article contents
	// behind in /tmp. Windows falls back to removal after the file is closed.
	if e := os.Remove(f.Name()); e != nil {
		defer os.Remove(f.Name())
	}
	defer f.Close()
	b := &responseBudget{dst: f, remaining: a.config.security().MaxResponseBytes}
	if e = produce(b); e != nil {
		return e
	}
	size, e := f.Seek(0, io.SeekCurrent)
	if e != nil {
		return e
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return e
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	_, e = io.Copy(w, f)
	return e
}
func writeArticleStart(w io.Writer, id string, updated int64) error {
	b, e := json.Marshal(map[string]any{"id": id, "updated": updated, "direction": "ltr", "title": "roi"})
	if e != nil {
		return e
	}
	if _, e = w.Write(b[:len(b)-1]); e != nil {
		return e
	}
	_, e = io.WriteString(w, `,"items":[`)
	return e
}
func writeArticle(w io.Writer, x Entry, count *int, limit int64) error {
	// Refuse oversized raw values before json.Encoder allocates its escape buffer.
	if int64(len(x.Title)+len(x.URL)+len(x.Content)+len(x.Author)+len(x.FeedTitle)+len(x.Site)) > limit {
		return errResponseLimit
	}
	if *count > 0 {
		if _, e := io.WriteString(w, ","); e != nil {
			return e
		}
	}
	if e := json.NewEncoder(w).Encode(x.wire()); e != nil {
		return e
	}
	*count++
	return nil
}
func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var x Entry
	e := row.Scan(&x.ID, &x.FeedID, &x.Title, &x.URL, &x.Content, &x.Author, &x.FeedTitle, &x.Site, &x.Published, &x.Received, &x.Updated, &x.Read, &x.Starred)
	return x, e
}
func (a *App) streamArticleContents(w http.ResponseWriter, r *http.Request, where string, args []any, order, streamID string, c cursor, n int) error {
	return a.articleResponse(w, func(dst io.Writer) error {
		tx, e := a.store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if e != nil {
			return e
		}
		defer tx.Rollback()
		rows, e := tx.QueryContext(r.Context(), "SELECT "+entryColumns+" FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE "+where+" ORDER BY e.id "+order+" LIMIT ?", args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		if e = writeArticleStart(dst, streamID, time.Now().Unix()); e != nil {
			return e
		}
		count := 0
		last := int64(0)
		more := false
		for rows.Next() {
			if count == n {
				more = true
				break
			}
			x, e := scanEntry(rows)
			if e != nil {
				return e
			}
			if e = writeArticle(dst, x, &count, a.config.security().MaxResponseBytes); e != nil {
				return e
			}
			last = x.ID
		}
		if e = rows.Err(); e != nil {
			return e
		}
		if _, e = io.WriteString(dst, "]"); e != nil {
			return e
		}
		if more {
			c.Last = last
			b, e := json.Marshal(a.encodeCursor(c))
			if e != nil {
				return e
			}
			if _, e = fmt.Fprintf(dst, `,"continuation":%s`, b); e != nil {
				return e
			}
		}
		_, e = io.WriteString(dst, "}\n")
		return e
	})
}
func (a *App) articleContents(w http.ResponseWriter, r *http.Request, ids []int64) error {
	return a.articleResponse(w, func(dst io.Writer) error {
		tx, e := a.store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if e = writeArticleStart(dst, "user/1/state/com.google/reading-list", time.Now().Unix()); e != nil {
			return e
		}
		count := 0
		for _, id := range ids {
			if e = r.Context().Err(); e != nil {
				return e
			}
			x, e := scanEntry(tx.QueryRowContext(r.Context(), "SELECT "+entryColumns+" FROM entries e JOIN feeds f ON f.id=e.feed_id WHERE e.id=? AND "+readerEntryFilter, id))
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			if e = writeArticle(dst, x, &count, a.config.security().MaxResponseBytes); e != nil {
				return e
			}
		}
		_, e = io.WriteString(dst, "]}\n")
		return e
	})
}
