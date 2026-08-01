package store

import "fmt"

func (s *Store) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			platform TEXT NOT NULL,
			token TEXT NOT NULL,
			base_url TEXT NOT NULL,
			user_id TEXT NOT NULL,
			credentials_json TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS sync_cursors (
			account_id TEXT PRIMARY KEY,
			buf TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT 'default',
			archived INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS user_preferences (
			user_id TEXT PRIMARY KEY,
			current_session_id TEXT NOT NULL DEFAULT '',
			model_name TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS workflow_requests (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tool_approvals (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			actor_open_id TEXT NOT NULL DEFAULT '',
			actor_user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL,
			source_message_id TEXT NOT NULL DEFAULT '',
			card_message_id TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tool_approval_grants (
			account_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			actor_type TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			source_request_id TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, tool_name, actor_type, actor_id, chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS feishu_chat_folders (
			account_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			folder_token TEXT NOT NULL,
			name TEXT NOT NULL,
			url TEXT NOT NULL DEFAULT '',
			parent_folder_token TEXT NOT NULL DEFAULT '',
			is_default INTEGER NOT NULL DEFAULT 0,
			share_member_type TEXT NOT NULL,
			share_member_id TEXT NOT NULL,
			share_state TEXT NOT NULL,
			create_request_id TEXT NOT NULL UNIQUE,
			created_by_open_id TEXT NOT NULL DEFAULT '',
			created_by_user_id TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, chat_id, folder_token)
		)`,
		`CREATE TABLE IF NOT EXISTS feishu_chat_documents (
			account_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			document_token TEXT NOT NULL,
			folder_token TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			source_request_id TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, chat_id, document_token)
		)`,
		`CREATE TABLE IF NOT EXISTS feishu_bot_resources (
			account_id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_token TEXT NOT NULL,
			parent_token TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			source_request_id TEXT NOT NULL DEFAULT '',
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, resource_type, resource_token)
		)`,
		`CREATE TABLE IF NOT EXISTS feishu_resource_access_requests (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			actor_open_id TEXT NOT NULL DEFAULT '',
			actor_user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL,
			source_message_id TEXT NOT NULL DEFAULT '',
			resource_type TEXT NOT NULL,
			resource_token TEXT NOT NULL,
			resource_url TEXT NOT NULL DEFAULT '',
			permission TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			subject_type TEXT NOT NULL DEFAULT '',
			subject_id TEXT NOT NULL DEFAULT '',
			grant_source TEXT NOT NULL DEFAULT '',
			verified_permission TEXT NOT NULL DEFAULT '',
			card_message_id TEXT NOT NULL DEFAULT '',
			oauth_state_hash TEXT NOT NULL DEFAULT '',
			pkce_verifier TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			expires_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS feishu_resource_grants (
			account_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_token TEXT NOT NULL,
			permission TEXT NOT NULL,
			subject_type TEXT NOT NULL,
			subject_id TEXT NOT NULL,
			source_request_id TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			verified_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, chat_id, resource_type, resource_token)
		)`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := s.renameToolApprovalGrantRequestIDColumn(); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO workflow_requests (id, account_id, kind, state, created_at_ms, updated_at_ms)
		 SELECT id, account_id, 'tool_approval', state, created_at_ms, updated_at_ms
		 FROM tool_approvals`,
	); err != nil {
		return fmt.Errorf("migrate workflow requests: %w", err)
	}
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO feishu_bot_resources (
			account_id, resource_type, resource_token, parent_token, name, url,
			source_request_id, created_at_ms, updated_at_ms
		) SELECT account_id, 'folder', folder_token, parent_folder_token, name, url,
			create_request_id, created_at_ms, updated_at_ms
		  FROM feishu_chat_folders`,
	); err != nil {
		return fmt.Errorf("backfill feishu bot folders: %w", err)
	}
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_archived ON sessions(user_id, archived)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_approvals_account_state_expiry
		 ON tool_approvals(account_id, state, expires_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_approval_grants_account_expiry
			 ON tool_approval_grants(account_id, expires_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_requests_account_kind_state
			 ON workflow_requests(account_id, kind, state, updated_at_ms)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_chat_folders_one_default
			 ON feishu_chat_folders(account_id, chat_id) WHERE is_default=1`,
		`CREATE INDEX IF NOT EXISTS idx_feishu_chat_folders_account_chat
			 ON feishu_chat_folders(account_id, chat_id, created_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_feishu_chat_documents_folder
			 ON feishu_chat_documents(account_id, chat_id, folder_token)`,
		`CREATE INDEX IF NOT EXISTS idx_feishu_bot_resources_account_type
			 ON feishu_bot_resources(account_id, resource_type, created_at_ms)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_feishu_resource_access_oauth_state
			 ON feishu_resource_access_requests(oauth_state_hash) WHERE oauth_state_hash<>''`,
		`CREATE INDEX IF NOT EXISTS idx_feishu_resource_access_account_state
			 ON feishu_resource_access_requests(account_id, state, expires_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_feishu_resource_grants_account_chat
			 ON feishu_resource_grants(account_id, chat_id, state, updated_at_ms)`,
	}
	for _, q := range indexes {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate indexes: %w", err)
		}
	}
	return nil
}

func (s *Store) renameToolApprovalGrantRequestIDColumn() error {
	rows, err := s.db.Query(`PRAGMA table_info(tool_approval_grants)`)
	if err != nil {
		return fmt.Errorf("inspect tool approval grant schema: %w", err)
	}
	defer rows.Close()
	hasRequestID := false
	hasApprovalID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan tool approval grant schema: %w", err)
		}
		switch name {
		case "source_request_id":
			hasRequestID = true
		case "source_approval_id":
			hasApprovalID = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect tool approval grant schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close tool approval grant schema inspection: %w", err)
	}
	if hasRequestID || !hasApprovalID {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE tool_approval_grants RENAME COLUMN source_approval_id TO source_request_id`); err != nil {
		return fmt.Errorf("rename tool approval grant source request column: %w", err)
	}
	return nil
}
