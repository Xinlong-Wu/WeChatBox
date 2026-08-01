package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"lingobridge/internal/config"
)

func TestWorkflowRequestUsesGloballyUniqueRootID(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	created, err := st.CreateWorkflowRequest(WorkflowRequest{
		AccountID: "feishu:cli_test",
		Kind:      "feishu_docs_folder_create",
		State:     WorkflowRequestStateExecuting,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	if !strings.HasPrefix(created.ID, "req_") || created.State != WorkflowRequestStateExecuting || !created.CreatedAt.Equal(now) {
		t.Fatalf("created request = %#v", created)
	}
	if _, err := st.CreateWorkflowRequest(WorkflowRequest{
		ID:        created.ID,
		AccountID: "feishu:other",
		Kind:      "other_workflow",
		State:     WorkflowRequestStatePending,
		CreatedAt: now,
	}); !errors.Is(err, ErrWorkflowRequestExists) {
		t.Fatalf("duplicate request error = %v, want ErrWorkflowRequestExists", err)
	}
	if err := st.UpdateWorkflowRequestState(created.ID, created.AccountID, WorkflowRequestStateSucceeded, now.Add(time.Second)); err != nil {
		t.Fatalf("UpdateWorkflowRequestState returned error: %v", err)
	}
	got, err := st.GetWorkflowRequest(created.ID, created.AccountID)
	if err != nil {
		t.Fatalf("GetWorkflowRequest returned error: %v", err)
	}
	if got.State != WorkflowRequestStateSucceeded || !got.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("updated request = %#v", got)
	}
}

func TestWorkflowMigrationBackfillsApprovalsAndRenamesGrantSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := config.EnsurePlatformDataDir(PlatformFeishu); err != nil {
		t.Fatalf("EnsurePlatformDataDir returned error: %v", err)
	}
	dbPath, err := config.PlatformDBPath(PlatformFeishu)
	if err != nil {
		t.Fatalf("PlatformDBPath returned error: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		`CREATE TABLE tool_approvals (
			id TEXT PRIMARY KEY, account_id TEXT NOT NULL, tool_name TEXT NOT NULL,
			actor_open_id TEXT NOT NULL DEFAULT '', actor_user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL, source_message_id TEXT NOT NULL DEFAULT '',
			card_message_id TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL, state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE tool_approval_grants (
			account_id TEXT NOT NULL, tool_name TEXT NOT NULL, actor_type TEXT NOT NULL,
			actor_id TEXT NOT NULL, chat_id TEXT NOT NULL, source_approval_id TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, tool_name, actor_type, actor_id, chat_id)
		)`,
		`INSERT INTO tool_approvals VALUES (
			'legacy_request', 'feishu:cli_test', 'feishu_docs_create', 'ou_requester', '',
			'oc_chat', 'om_source', 'om_card', '{}', 'pending', 1, 60001, 1
		)`,
		`INSERT INTO tool_approval_grants VALUES (
			'feishu:cli_test', 'feishu_docs_create', 'open_id', 'ou_requester', 'oc_chat',
			'legacy_request', 1, 60001, 1
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("prepare legacy schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open migrated store returned error: %v", err)
	}
	defer st.Close()
	workflow, err := st.GetWorkflowRequest("legacy_request", "feishu:cli_test")
	if err != nil || workflow.Kind != WorkflowRequestKindToolApproval || workflow.State != WorkflowRequestStatePending {
		t.Fatalf("backfilled workflow = %#v err=%v", workflow, err)
	}
	var sourceRequestID string
	if err := st.db.QueryRow(`SELECT source_request_id FROM tool_approval_grants`).Scan(&sourceRequestID); err != nil {
		t.Fatalf("query renamed grant source: %v", err)
	}
	if sourceRequestID != "legacy_request" {
		t.Fatalf("source_request_id = %q, want legacy_request", sourceRequestID)
	}
}
