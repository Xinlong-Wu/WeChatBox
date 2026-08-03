package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// CreateSession creates a new session for a user and sets it as current.
func (s *Store) CreateSession(userID, name string) (*Session, error) {
	if name == "" {
		name = "default"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sess, err := createSessionTx(tx, userID, name)
	if err != nil {
		return nil, err
	}
	if err := upsertCurrentSessionTx(tx, userID, sess.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	sess.Current = true
	return sess, nil
}

// GetCurrentSession returns the current session for a user, or creates a default one.
func (s *Store) GetCurrentSession(userID string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sess, err := currentSessionTx(tx, userID)
	if errors.Is(err, ErrSessionNotFound) {
		sess, err = ensureCurrentSessionTx(tx, userID)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sess, nil
}

// ListSessions returns all unarchived sessions for a user.
func (s *Store) ListSessions(userID string) ([]Session, error) {
	currentID, err := s.getCurrentSessionID(userID)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, user_id, name, model_name, archived, created_at FROM sessions
		 WHERE user_id=? AND archived=0 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Name, &sess.ModelName, &sess.Archived, &sess.CreatedAt); err != nil {
			return nil, err
		}
		sess.Current = sess.ID == currentID
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// SwitchSession sets an unarchived session as current.
func (s *Store) SwitchSession(userID, sessionName string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sess, err := sessionByNameTx(tx, userID, sessionName)
	if err != nil {
		return nil, err
	}
	if err := upsertCurrentSessionTx(tx, userID, sess.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sess.Current = true
	return sess, nil
}

// RenameSession renames an unarchived session.
func (s *Store) RenameSession(userID, sessionID, newName string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sess, err := sessionByIDTx(tx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if err := checkSessionNameAvailableTx(tx, userID, newName, sessionID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET name=? WHERE id=? AND user_id=?`, newName, sessionID, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sess.Name = newName
	currentID, err := s.getCurrentSessionID(userID)
	if err != nil {
		return nil, err
	}
	sess.Current = sess.ID == currentID
	return sess, nil
}

// ArchiveSession marks an unarchived session as archived and clears it as current.
func (s *Store) ArchiveSession(userID, sessionName string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sess, err := sessionByNameTx(tx, userID, sessionName)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET archived=1 WHERE id=? AND user_id=?`, sess.ID, userID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`UPDATE user_preferences SET current_session_id='', updated_at=datetime('now')
		 WHERE user_id=? AND current_session_id=?`,
		userID, sess.ID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sess.Archived = true
	sess.Current = false
	return sess, nil
}

// ClearCurrentSession archives the current session and creates a new current session.
func (s *Store) ClearCurrentSession(userID, newName string) (*Session, error) {
	if newName == "" {
		newName = "default"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	current, err := currentSessionTx(tx, userID)
	if errors.Is(err, ErrSessionNotFound) {
		current, err = ensureCurrentSessionTx(tx, userID)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE sessions SET archived=1 WHERE id=? AND user_id=?`, current.ID, userID); err != nil {
		return nil, err
	}

	next, err := createSessionTx(tx, userID, newName)
	if err != nil {
		return nil, err
	}
	if err := upsertCurrentSessionTx(tx, userID, next.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	next.Current = true
	return next, nil
}

func createSessionTx(tx *sql.Tx, userID, name string) (*Session, error) {
	if err := checkSessionNameAvailableTx(tx, userID, name, ""); err != nil {
		return nil, err
	}
	id, err := generateID()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO sessions (id, user_id, name, archived) VALUES (?, ?, ?, 0)`,
		id, userID, name,
	); err != nil {
		return nil, err
	}
	return sessionByIDTx(tx, userID, id)
}

func checkSessionNameAvailableTx(tx *sql.Tx, userID, name, exceptID string) error {
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE user_id=? AND name=? AND archived=0 AND id<>?`,
		userID, name, exceptID,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: %q for user %s", ErrSessionExists, name, userID)
	}
	return nil
}

func currentSessionTx(tx *sql.Tx, userID string) (*Session, error) {
	var currentID string
	err := tx.QueryRow(`SELECT current_session_id FROM user_preferences WHERE user_id=?`, userID).Scan(&currentID)
	if err == sql.ErrNoRows || currentID == "" {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	sess, err := sessionByIDTx(tx, userID, currentID)
	if err != nil {
		return nil, err
	}
	sess.Current = true
	return sess, nil
}

func ensureCurrentSessionTx(tx *sql.Tx, userID string) (*Session, error) {
	sess, err := latestUnarchivedSessionTx(tx, userID)
	if errors.Is(err, ErrSessionNotFound) {
		sess, err = createSessionTx(tx, userID, "default")
	}
	if err != nil {
		return nil, err
	}
	if err := upsertCurrentSessionTx(tx, userID, sess.ID); err != nil {
		return nil, err
	}
	sess.Current = true
	return sess, nil
}

func latestUnarchivedSessionTx(tx *sql.Tx, userID string) (*Session, error) {
	var sess Session
	err := tx.QueryRow(
		`SELECT id, user_id, name, model_name, archived, created_at FROM sessions
		 WHERE user_id=? AND archived=0 ORDER BY created_at DESC, id DESC LIMIT 1`,
		userID,
	).Scan(&sess.ID, &sess.UserID, &sess.Name, &sess.ModelName, &sess.Archived, &sess.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	return &sess, err
}

func sessionByNameTx(tx *sql.Tx, userID, name string) (*Session, error) {
	var sess Session
	err := tx.QueryRow(
		`SELECT id, user_id, name, model_name, archived, created_at FROM sessions
		 WHERE user_id=? AND name=? AND archived=0`,
		userID, name,
	).Scan(&sess.ID, &sess.UserID, &sess.Name, &sess.ModelName, &sess.Archived, &sess.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %q for user %s", ErrSessionNotFound, name, userID)
	}
	return &sess, err
}

func sessionByIDTx(tx *sql.Tx, userID, sessionID string) (*Session, error) {
	var sess Session
	err := tx.QueryRow(
		`SELECT id, user_id, name, model_name, archived, created_at FROM sessions
		 WHERE user_id=? AND id=? AND archived=0`,
		userID, sessionID,
	).Scan(&sess.ID, &sess.UserID, &sess.Name, &sess.ModelName, &sess.Archived, &sess.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	return &sess, err
}
