package roi

import (
	"database/sql"
	"fmt"
)

func migrate(db *sql.DB) error {
	var err error
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 15 {
		return fmt.Errorf("database schema %d is newer than supported schema 15", version)
	}
	if version == 0 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		_, err = tx.Exec(`
 CREATE TABLE feeds(id INTEGER PRIMARY KEY AUTOINCREMENT,url TEXT NOT NULL UNIQUE,title TEXT NOT NULL,site TEXT NOT NULL DEFAULT '',active INTEGER NOT NULL DEFAULT 1,paused INTEGER NOT NULL DEFAULT 0,etag TEXT NOT NULL DEFAULT '',modified TEXT NOT NULL DEFAULT '',next_fetch INTEGER NOT NULL DEFAULT 0,last_attempt INTEGER NOT NULL DEFAULT 0,last_success INTEGER NOT NULL DEFAULT 0,last_new INTEGER NOT NULL DEFAULT 0,last_published INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',failures INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE folders(name TEXT PRIMARY KEY);
 CREATE TABLE feed_folders(feed_id INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,name TEXT NOT NULL REFERENCES folders(name) ON DELETE CASCADE,PRIMARY KEY(feed_id,name));
 -- received is Unix microseconds; published and updated are Unix seconds.
 CREATE TABLE entries(id INTEGER PRIMARY KEY AUTOINCREMENT,feed_id INTEGER NOT NULL REFERENCES feeds(id),guid TEXT NOT NULL,title TEXT NOT NULL,url TEXT NOT NULL,content TEXT NOT NULL,author TEXT NOT NULL,published INTEGER NOT NULL,received INTEGER NOT NULL,updated INTEGER NOT NULL,is_read INTEGER NOT NULL DEFAULT 0,starred INTEGER NOT NULL DEFAULT 0,UNIQUE(feed_id,guid));
 CREATE INDEX entries_feed_time ON entries(feed_id,received DESC,id DESC);
 CREATE INDEX entries_unread ON entries(is_read,feed_id);
 CREATE INDEX entries_starred ON entries(starred,id);
 CREATE TABLE tombstones(feed_id INTEGER NOT NULL REFERENCES feeds(id),guid TEXT NOT NULL,PRIMARY KEY(feed_id,guid));
 CREATE TABLE fetches(id INTEGER PRIMARY KEY AUTOINCREMENT,feed_id INTEGER NOT NULL REFERENCES feeds(id),started INTEGER NOT NULL,finished INTEGER NOT NULL,status INTEGER NOT NULL,added INTEGER NOT NULL,error TEXT NOT NULL);
 CREATE INDEX fetches_feed ON fetches(feed_id,id DESC);
 CREATE TABLE maintenance(id INTEGER PRIMARY KEY CHECK(id=1),last_run INTEGER NOT NULL,deleted INTEGER NOT NULL);
 PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if version < 2 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`CREATE TABLE auth_secrets(name TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE login_limits(key TEXT PRIMARY KEY,window INTEGER NOT NULL,hits INTEGER NOT NULL);
 PRAGMA user_version=2;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 3 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`CREATE TABLE IF NOT EXISTS auth_sessions(id TEXT PRIMARY KEY,realm TEXT NOT NULL,created INTEGER NOT NULL,expires INTEGER NOT NULL,last_seen INTEGER NOT NULL,first_ip TEXT NOT NULL,last_ip TEXT NOT NULL,client TEXT NOT NULL,requests INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS auth_events(id INTEGER PRIMARY KEY,created INTEGER NOT NULL,realm TEXT NOT NULL,session_id TEXT NOT NULL,method TEXT NOT NULL,path TEXT NOT NULL,ip TEXT NOT NULL,client TEXT NOT NULL,status INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS auth_events_session ON auth_events(session_id,id);
CREATE TRIGGER IF NOT EXISTS auth_events_bound AFTER INSERT ON auth_events BEGIN DELETE FROM auth_events WHERE id<=NEW.id-10000; END;
PRAGMA user_version=3;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 4 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE auth_sessions ADD COLUMN approved INTEGER NOT NULL DEFAULT 0;
ALTER TABLE auth_sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT '';
PRAGMA user_version=4;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 5 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE auth_events ADD COLUMN device_name TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_events ADD COLUMN device_state TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_events ADD COLUMN target_session TEXT NOT NULL DEFAULT '';
PRAGMA user_version=5;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 6 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE feeds ADD COLUMN git_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE feeds ADD COLUMN git_repo TEXT NOT NULL DEFAULT '';
ALTER TABLE feeds ADD COLUMN git_mode TEXT NOT NULL DEFAULT 'releases';
ALTER TABLE feeds ADD COLUMN git_preview INTEGER NOT NULL DEFAULT 0;
ALTER TABLE feeds ADD COLUMN git_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE feeds ADD COLUMN icon_url TEXT NOT NULL DEFAULT '';
ALTER TABLE entries ADD COLUMN git_preview INTEGER NOT NULL DEFAULT 0;
INSERT OR IGNORE INTO folders(name) VALUES('Imported/GIT_Releases');
INSERT OR IGNORE INTO feed_folders(feed_id,name) SELECT feed_id,'Imported/GIT_Releases' FROM feed_folders WHERE name='GIT_Releases';
DELETE FROM feed_folders WHERE name='GIT_Releases';
DELETE FROM folders WHERE name='Imported/GIT_Releases' AND NOT EXISTS(SELECT 1 FROM feed_folders WHERE name='Imported/GIT_Releases');
INSERT OR IGNORE INTO folders(name) VALUES('GIT_Releases');
CREATE TRIGGER git_folder_only BEFORE INSERT ON feed_folders WHEN NEW.name='GIT_Releases' AND (SELECT git_provider FROM feeds WHERE id=NEW.feed_id)='' BEGIN SELECT RAISE(ABORT,'GIT_Releases accepts only Git sources'); END;
PRAGMA user_version=6;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 7 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`CREATE TABLE IF NOT EXISTS host_limits(host TEXT PRIMARY KEY,blocked_until INTEGER NOT NULL); PRAGMA user_version=7;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 8 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE auth_sessions ADD COLUMN csrf TEXT NOT NULL DEFAULT '';
UPDATE auth_sessions SET revoked=1 WHERE realm='admin';
PRAGMA user_version=8;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 9 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE feeds ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''; PRAGMA user_version=9;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}

	if version < 10 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`ALTER TABLE feeds ADD COLUMN git_tag_filter TEXT NOT NULL DEFAULT ''; PRAGMA user_version=10;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 11 {
		if err = migrateReview(db); err != nil {
			return err
		}
	}
	if version < 12 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec("ALTER TABLE auth_sessions ADD COLUMN feeds_hidden INTEGER NOT NULL DEFAULT 0; PRAGMA user_version=12;"); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 13 {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.Exec(`CREATE TABLE api_devices (
id INTEGER PRIMARY KEY AUTOINCREMENT, login TEXT NOT NULL UNIQUE,
name TEXT NOT NULL, code_hash TEXT NOT NULL DEFAULT '',
code_expires INTEGER NOT NULL DEFAULT 0, created INTEGER NOT NULL);
ALTER TABLE auth_sessions ADD COLUMN device_id INTEGER NOT NULL DEFAULT 0;
CREATE INDEX auth_sessions_device ON auth_sessions(device_id);
PRAGMA user_version=13;`); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if version < 14 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`CREATE TABLE backup_days (
 day TEXT PRIMARY KEY, attempts INTEGER NOT NULL DEFAULT 0,
 created INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0,
 errors INTEGER NOT NULL DEFAULT 0, last_attempt INTEGER NOT NULL,
 last_finished INTEGER NOT NULL, last_success INTEGER NOT NULL DEFAULT 0,
 duration_ms INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');
 PRAGMA user_version=14;`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if version < 15 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`CREATE TABLE subscription_backup_state(directory TEXT PRIMARY KEY,checked_day TEXT NOT NULL DEFAULT '',created_day TEXT NOT NULL DEFAULT '',last_checked INTEGER NOT NULL DEFAULT 0,last_created INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL DEFAULT 0,bytes INTEGER NOT NULL DEFAULT 0,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT ''); PRAGMA user_version=15;`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
