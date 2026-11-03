package roi

// ResetFeed removes downloaded state while keeping the subscription identity,
// source options and folder membership. Its revision invalidates in-flight RSS
// and Git responses so they cannot restore the state being discarded.
func (s *Store) ResetFeed(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE feeds SET active=1,paused=0,etag='',modified='',next_fetch=0,last_attempt=0,last_success=0,last_new=0,last_published=0,error='',failures=0,git_revision=git_revision+1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrFeedNotFound
	}
	// Clearing tombstones permits previously rotated articles to be fetched again.
	for _, table := range []string{"entries", "tombstones", "fetches"} {
		if _, err = tx.Exec("DELETE FROM "+table+" WHERE feed_id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
