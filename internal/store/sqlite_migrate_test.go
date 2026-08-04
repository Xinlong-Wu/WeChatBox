package store

import "testing"

func TestMigrateAddsFeishuResourceAccessDisplayAndDecisionColumnsAndRemovesLegacyConsumption(t *testing.T) {
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
	for _, column := range []string{"resource_display_name", "once_duration_minutes", "grant_mode", "decision_at_ms"} {
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
		!columns["resource_display_name"] || !columns["once_duration_minutes"] || !columns["grant_mode"] || !columns["decision_at_ms"] {
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
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN (?, ?)`,
		"idx_feishu_oauth_refresh_one_active",
		"idx_feishu_oauth_refresh_account_state_lease",
	).Scan(&indexCount); err != nil {
		t.Fatalf("inspect refresh attempt indexes: %v", err)
	}
	if indexCount != 2 {
		t.Fatalf("refresh attempt index count = %d, want 2", indexCount)
	}
}
