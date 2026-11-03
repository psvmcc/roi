package roi

import "database/sql"

// Derived counters are updated in the same transaction as article state. This
// keeps dashboards and Reader metadata independent of the size of the archive.
func migrateReview(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
ALTER TABLE entries ADD COLUMN git_object TEXT NOT NULL DEFAULT '';
ALTER TABLE entries ADD COLUMN render_version INTEGER NOT NULL DEFAULT 0;
CREATE INDEX entries_feed_published ON entries(feed_id,published DESC);
CREATE INDEX entries_updated ON entries(updated,id);
CREATE TABLE git_http_cache(key TEXT PRIMARY KEY,etag TEXT NOT NULL,modified TEXT NOT NULL,body BLOB NOT NULL,next_page INTEGER NOT NULL,checked INTEGER NOT NULL);
CREATE INDEX git_cache_checked ON git_http_cache(checked);
CREATE TABLE feed_stats(feed_id INTEGER PRIMARY KEY REFERENCES feeds(id) ON DELETE CASCADE,count INTEGER NOT NULL DEFAULT 0,unread INTEGER NOT NULL DEFAULT 0,visible_unread INTEGER NOT NULL DEFAULT 0,bytes INTEGER NOT NULL DEFAULT 0,tombstones INTEGER NOT NULL DEFAULT 0,key_bytes INTEGER NOT NULL DEFAULT 0);
INSERT INTO feed_stats(feed_id,count,unread,visible_unread,bytes) SELECT f.id,COUNT(e.id),COALESCE(SUM(e.is_read=0),0),COALESCE(SUM(e.is_read=0 AND (f.git_preview=1 OR e.git_preview=0) AND (f.git_tag_filter='' OR substr(e.guid,5) GLOB f.git_tag_filter)),0),COALESCE(SUM(length(CAST(e.title AS BLOB))+length(CAST(e.url AS BLOB))+length(CAST(e.content AS BLOB))+length(CAST(e.author AS BLOB))+length(CAST(e.guid AS BLOB))),0) FROM feeds f LEFT JOIN entries e ON e.feed_id=f.id GROUP BY f.id;
UPDATE feed_stats SET tombstones=(SELECT COUNT(*) FROM tombstones WHERE feed_id=feed_stats.feed_id),key_bytes=COALESCE((SELECT SUM(length(CAST(guid AS BLOB))) FROM tombstones WHERE feed_id=feed_stats.feed_id),0);
CREATE TRIGGER tombstone_stats_insert AFTER INSERT ON tombstones BEGIN UPDATE feed_stats SET tombstones=tombstones+1,key_bytes=key_bytes+length(CAST(NEW.guid AS BLOB)) WHERE feed_id=NEW.feed_id; END;
CREATE TRIGGER tombstone_stats_delete AFTER DELETE ON tombstones BEGIN UPDATE feed_stats SET tombstones=tombstones-1,key_bytes=key_bytes-length(CAST(OLD.guid AS BLOB)) WHERE feed_id=OLD.feed_id; END;
CREATE TRIGGER feed_stats_insert AFTER INSERT ON feeds BEGIN INSERT INTO feed_stats(feed_id) VALUES(NEW.id); END;
CREATE TRIGGER entry_stats_insert AFTER INSERT ON entries BEGIN
 UPDATE feed_stats SET count=count+1,unread=unread+(NEW.is_read=0),visible_unread=visible_unread+(NEW.is_read=0 AND EXISTS(SELECT 1 FROM feeds WHERE id=NEW.feed_id AND (git_preview=1 OR NEW.git_preview=0) AND (git_tag_filter='' OR substr(NEW.guid,5) GLOB git_tag_filter))),bytes=bytes+length(CAST(NEW.title AS BLOB))+length(CAST(NEW.url AS BLOB))+length(CAST(NEW.content AS BLOB))+length(CAST(NEW.author AS BLOB))+length(CAST(NEW.guid AS BLOB)) WHERE feed_id=NEW.feed_id;
END;
CREATE TRIGGER entry_stats_delete AFTER DELETE ON entries BEGIN
 UPDATE feed_stats SET count=count-1,unread=unread-(OLD.is_read=0),visible_unread=visible_unread-(OLD.is_read=0 AND EXISTS(SELECT 1 FROM feeds WHERE id=OLD.feed_id AND (git_preview=1 OR OLD.git_preview=0) AND (git_tag_filter='' OR substr(OLD.guid,5) GLOB git_tag_filter))),bytes=bytes-length(CAST(OLD.title AS BLOB))-length(CAST(OLD.url AS BLOB))-length(CAST(OLD.content AS BLOB))-length(CAST(OLD.author AS BLOB))-length(CAST(OLD.guid AS BLOB)) WHERE feed_id=OLD.feed_id;
END;
CREATE TRIGGER entry_stats_update AFTER UPDATE OF feed_id,title,url,content,author,guid,is_read,git_preview ON entries BEGIN
 UPDATE feed_stats SET count=count-1,unread=unread-(OLD.is_read=0),visible_unread=visible_unread-(OLD.is_read=0 AND EXISTS(SELECT 1 FROM feeds WHERE id=OLD.feed_id AND (git_preview=1 OR OLD.git_preview=0) AND (git_tag_filter='' OR substr(OLD.guid,5) GLOB git_tag_filter))),bytes=bytes-length(CAST(OLD.title AS BLOB))-length(CAST(OLD.url AS BLOB))-length(CAST(OLD.content AS BLOB))-length(CAST(OLD.author AS BLOB))-length(CAST(OLD.guid AS BLOB)) WHERE feed_id=OLD.feed_id;
 UPDATE feed_stats SET count=count+1,unread=unread+(NEW.is_read=0),visible_unread=visible_unread+(NEW.is_read=0 AND EXISTS(SELECT 1 FROM feeds WHERE id=NEW.feed_id AND (git_preview=1 OR NEW.git_preview=0) AND (git_tag_filter='' OR substr(NEW.guid,5) GLOB git_tag_filter))),bytes=bytes+length(CAST(NEW.title AS BLOB))+length(CAST(NEW.url AS BLOB))+length(CAST(NEW.content AS BLOB))+length(CAST(NEW.author AS BLOB))+length(CAST(NEW.guid AS BLOB)) WHERE feed_id=NEW.feed_id;
END;
CREATE TRIGGER feed_stats_filter AFTER UPDATE OF git_preview,git_tag_filter ON feeds WHEN OLD.git_preview<>NEW.git_preview OR OLD.git_tag_filter<>NEW.git_tag_filter BEGIN
 UPDATE feed_stats SET visible_unread=(SELECT COUNT(*) FROM entries WHERE feed_id=NEW.id AND is_read=0 AND (NEW.git_preview=1 OR git_preview=0) AND (NEW.git_tag_filter='' OR substr(guid,5) GLOB NEW.git_tag_filter)) WHERE feed_id=NEW.id;
END;
PRAGMA user_version=11;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
