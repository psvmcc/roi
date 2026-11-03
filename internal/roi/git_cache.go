package roi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

const gitRenderVersion = 2
const gitCacheBytes = 128 << 20
const gitCacheEntries = 2048

type gitCacheScope struct{}
type gitCacheEntry struct {
	ETag, Modified string
	Body           []byte
	Next           bool
}

func gitCacheKey(ctx context.Context, raw, token string) string {
	credential := sha256.Sum256([]byte(token))
	scope, _ := ctx.Value(gitCacheScope{}).(string)
	hash := sha256.Sum256([]byte(raw + "\n" + hex.EncodeToString(credential[:]) + "\n" + scope))
	return hex.EncodeToString(hash[:])
}
func (p *Poller) cachedGit(ctx context.Context, key string) (gitCacheEntry, error) {
	var c gitCacheEntry
	err := p.store.DB.QueryRowContext(ctx, "SELECT etag,modified,body,next_page FROM git_http_cache WHERE key=? AND checked>?", key, time.Now().AddDate(0, 0, -7).Unix()).Scan(&c.ETag, &c.Modified, &c.Body, &c.Next)
	return c, err
}
func (p *Poller) cacheGit(ctx context.Context, key string, c gitCacheEntry) error {
	if c.ETag == "" && c.Modified == "" {
		return nil
	}
	if len(c.ETag) > 4096 || len(c.Modified) > 128 {
		return nil
	}
	if c.Body == nil {
		c.Body = []byte{}
	}
	tx, err := p.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO git_http_cache(key,etag,modified,body,next_page,checked) VALUES(?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET etag=excluded.etag,modified=excluded.modified,body=excluded.body,next_page=excluded.next_page,checked=excluded.checked`, key, c.ETag, c.Modified, c.Body, c.Next, time.Now().Unix()); err != nil {
		return err
	}
	// Bound both age and total cached bytes. Expired data is never authoritative.
	if _, err = tx.ExecContext(ctx, `DELETE FROM git_http_cache WHERE checked<? OR key IN (SELECT key FROM (SELECT key,SUM(length(body)+length(CAST(etag AS BLOB))+length(CAST(modified AS BLOB))+length(key)+256) OVER(ORDER BY checked DESC,key) AS bytes,ROW_NUMBER() OVER(ORDER BY checked DESC,key) AS position FROM git_http_cache) WHERE bytes>? OR position>?)`, time.Now().AddDate(0, 0, -7).Unix(), gitCacheBytes, gitCacheEntries); err != nil {
		return err
	}
	return tx.Commit()
}
func gitCacheMissing(err error) bool     { return err == sql.ErrNoRows }
func gitScope(id, revision int64) string { return fmt.Sprintf("%d:%d", id, revision) }
