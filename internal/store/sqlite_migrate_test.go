package store

import "testing"

func TestMigrateCreatesConversationFileLockSchema(t *testing.T) {
	st := openTestStore(t)
	rows, err := st.db.Query(`PRAGMA table_info(conversation_file_locks)`)
	if err != nil {
		t.Fatalf("inspect conversation file lock schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan conversation file lock schema: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{"user_id", "session_id", "generation"} {
		if !columns[name] {
			t.Fatalf("conversation_file_locks missing column %q: %#v", name, columns)
		}
	}
}

func TestMigrateAddsFeishuResourceAccessDisplayDecisionAndOAuthDeliveryColumnsAndRemovesLegacyConsumption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if _, err := st.db.Exec(`ALTER TABLE feishu_resource_access_requests ADD COLUMN consumed_by_request_id TEXT NOT NULL DEFAULT ''`); err != nil {
		st.Close()
		t.Fatalf("add legacy consumed_by_request_id: %v", err)
	}
	if _, err := st.db.Exec(`ALTER TABLE feishu_resource_access_requests ADD COLUMN consumed_at_ms INTEGER NOT NULL DEFAULT 0`); err != nil {
		st.Close()
		t.Fatalf("add legacy consumed_at_ms: %v", err)
	}
	for _, column := range []string{
		"resource_display_name", "once_duration_minutes", "grant_mode", "decision_at_ms",
		"oauth_state_ciphertext", "oauth_handoff_delivered_at_ms",
	} {
		if _, err := st.db.Exec(`ALTER TABLE feishu_resource_access_requests DROP COLUMN ` + column); err != nil {
			st.Close()
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close legacy-shaped store: %v", err)
	}

	migrated, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open migrated store returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := migrated.Close(); err != nil {
			t.Fatalf("close migrated store: %v", err)
		}
	})
	rows, err := migrated.db.Query(`PRAGMA table_info(feishu_resource_access_requests)`)
	if err != nil {
		t.Fatalf("inspect migrated schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan migrated schema: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("inspect migrated schema: %v", err)
	}
	if columns["consumed_by_request_id"] || columns["consumed_at_ms"] ||
		!columns["resource_display_name"] || !columns["once_duration_minutes"] || !columns["grant_mode"] || !columns["decision_at_ms"] ||
		!columns["oauth_state_ciphertext"] || !columns["oauth_handoff_delivered_at_ms"] {
		t.Fatalf("migrated columns = %#v", columns)
	}
}

func TestMigrateCreatesFeishuOAuthRefreshAttemptSchema(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	rows, err := st.db.Query(`PRAGMA table_info(feishu_oauth_refresh_attempts)`)
	if err != nil {
		t.Fatalf("inspect refresh attempt schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan refresh attempt schema: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate refresh attempt schema: %v", err)
	}
	for _, name := range []string{
		"attempt_id", "credential_id", "account_id", "expected_version", "state",
		"lease_token", "lease_expires_at_ms", "access_token_ciphertext",
		"refresh_token_ciphertext", "scopes", "error_category", "created_at_ms", "updated_at_ms",
	} {
		if !columns[name] {
			t.Fatalf("refresh attempt schema missing %q: %#v", name, columns)
		}
	}
	var indexCount int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN (?, ?, ?)`,
		"idx_feishu_oauth_refresh_one_active",
		"idx_feishu_oauth_refresh_account_state_lease",
		"idx_feishu_oauth_refresh_account_state_updated",
	).Scan(&indexCount); err != nil {
		t.Fatalf("inspect refresh attempt indexes: %v", err)
	}
	if indexCount != 3 {
		t.Fatalf("refresh attempt index count = %d, want 3", indexCount)
	}
}

func TestMigrateCreatesFeishuAccountRuntimeLeaseSchema(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	rows, err := st.db.Query(`PRAGMA table_info(feishu_account_runtime_leases)`)
	if err != nil {
		t.Fatalf("inspect account runtime lease schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan account runtime lease schema: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate account runtime lease schema: %v", err)
	}
	for _, name := range []string{"account_id", "owner_id", "acquired_at_ms", "heartbeat_at_ms", "lease_expires_at_ms"} {
		if !columns[name] {
			t.Fatalf("account runtime lease schema missing %q: %#v", name, columns)
		}
	}
}

func TestMigrateCreatesFeishuCardDeliverySchema(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	rows, err := st.db.Query(`PRAGMA table_info(feishu_card_deliveries)`)
	if err != nil {
		t.Fatalf("inspect card delivery schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan card delivery schema: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{
		"id", "account_id", "request_id", "purpose", "revision", "card_message_id",
		"state", "attempts", "available_at_ms", "lease_token", "lease_expires_at_ms",
		"last_error", "expires_at_ms", "created_at_ms", "updated_at_ms", "delivered_at_ms",
	} {
		if !columns[name] {
			t.Fatalf("feishu_card_deliveries missing column %q: %#v", name, columns)
		}
	}
}

func TestMigrateCreatesFeishuRemoteOperationSchema(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	rows, err := st.db.Query(`PRAGMA table_info(feishu_remote_operations)`)
	if err != nil {
		t.Fatalf("inspect remote operation schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan remote operation schema: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{
		"request_id", "account_id", "operation_kind", "chat_id", "actor_open_id", "actor_user_id",
		"parent_resource_type", "parent_resource_token", "binding_parent_token", "requested_name",
		"payload_hash", "set_default", "share_member_type", "share_member_id", "initial_content_requested",
		"state", "remote_resource_type", "remote_resource_token", "remote_url",
		"remote_call_started_at_ms", "remote_result_at_ms", "last_error_category", "created_at_ms", "updated_at_ms",
	} {
		if !columns[name] {
			t.Fatalf("feishu_remote_operations missing column %q: %#v", name, columns)
		}
	}
	var indexCount int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`,
		"idx_feishu_remote_operations_account_state",
	).Scan(&indexCount); err != nil {
		t.Fatalf("inspect remote operation index: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("remote operation index count = %d, want 1", indexCount)
	}
}
