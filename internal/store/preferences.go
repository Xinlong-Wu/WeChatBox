package store

import "database/sql"

// GetSessionModelName returns the stored model preference for one active session.
func (s *Store) GetSessionModelName(userID, sessionID string) (string, error) {
	var modelName string
	err := s.db.QueryRow(
		`SELECT model_name FROM sessions WHERE user_id=? AND id=? AND archived=0`,
		userID, sessionID,
	).Scan(&modelName)
	if err == sql.ErrNoRows {
		return "", ErrSessionNotFound
	}
	return modelName, err
}

// SetSessionModelName saves a model preference for one active session.
func (s *Store) SetSessionModelName(userID, sessionID, modelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE sessions SET model_name=? WHERE user_id=? AND id=? AND archived=0`,
		modelName, userID, sessionID,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// ResetUnavailableSessionModels resets session model preferences not present in validModels.
func (s *Store) ResetUnavailableSessionModels(defaultModel string, validModels []string) (int, error) {
	valid := map[string]bool{}
	for _, name := range validModels {
		valid[name] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, model_name FROM sessions WHERE model_name<>''`)
	if err != nil {
		return 0, err
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID, modelName string
		if err := rows.Scan(&sessionID, &modelName); err != nil {
			rows.Close()
			return 0, err
		}
		if !valid[modelName] {
			sessionIDs = append(sessionIDs, sessionID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, sessionID := range sessionIDs {
		if _, err := tx.Exec(
			`UPDATE sessions SET model_name=? WHERE id=?`,
			defaultModel, sessionID,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(sessionIDs), nil
}

func upsertCurrentSessionTx(tx *sql.Tx, userID, sessionID string) error {
	_, err := tx.Exec(
		`INSERT INTO user_preferences (user_id, current_session_id, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(user_id) DO UPDATE SET current_session_id=excluded.current_session_id, updated_at=excluded.updated_at`,
		userID, sessionID,
	)
	return err
}

func (s *Store) getCurrentSessionID(userID string) (string, error) {
	var currentID string
	err := s.db.QueryRow(`SELECT current_session_id FROM user_preferences WHERE user_id=?`, userID).Scan(&currentID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return currentID, err
}
