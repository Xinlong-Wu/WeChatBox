package store

import "testing"

func TestMigrateAddsFeishuResourceAccessDecisionColumnsAndRemovesLegacyConsumption(t *testing.T) {
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
	for _, column := range []string{"once_duration_minutes", "grant_mode", "decision_at_ms"} {
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
		!columns["once_duration_minutes"] || !columns["grant_mode"] || !columns["decision_at_ms"] {
		t.Fatalf("migrated columns = %#v", columns)
	}
}
