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
		`CREATE TABLE feishu_chat_folders (
			account_id TEXT NOT NULL, chat_id TEXT NOT NULL, folder_token TEXT NOT NULL,
			name TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', parent_folder_token TEXT NOT NULL DEFAULT '',
			is_default INTEGER NOT NULL DEFAULT 0, share_member_type TEXT NOT NULL,
			share_member_id TEXT NOT NULL, share_state TEXT NOT NULL,
			create_request_id TEXT NOT NULL UNIQUE, created_by_open_id TEXT NOT NULL DEFAULT '',
			created_by_user_id TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, chat_id, folder_token)
		)`,
		`CREATE TABLE feishu_resource_grants (
			account_id TEXT NOT NULL, chat_id TEXT NOT NULL, resource_type TEXT NOT NULL,
			resource_token TEXT NOT NULL, permission TEXT NOT NULL, subject_type TEXT NOT NULL,
			subject_id TEXT NOT NULL, source_request_id TEXT NOT NULL, state TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL, verified_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (account_id, chat_id, resource_type, resource_token)
		)`,
		`INSERT INTO tool_approvals VALUES (
			'legacy_request', 'feishu:cli_test', 'feishu_docs_create', 'ou_requester', '',
			'oc_chat', 'om_source', 'om_card', '{}', 'pending', 1, 60001, 1
		)`,
		`INSERT INTO tool_approval_grants VALUES (
			'feishu:cli_test', 'feishu_docs_create', 'open_id', 'ou_requester', 'oc_chat',
			'legacy_request', 1, 60001, 1
		)`,
		`INSERT INTO feishu_chat_folders VALUES (
			'feishu:cli_test', 'oc_chat', 'fld_legacy', 'Legacy Folder',
			'https://docs.feishu.cn/drive/folder/fld_legacy', '', 1, 'openchat', 'oc_chat',
			'succeeded', 'req_folder', 'ou_requester', 'u_requester', 1, 1
		)`,
		`INSERT INTO feishu_resource_grants VALUES (
			'feishu:cli_test', 'oc_chat', 'docx', 'doxcn_legacy', 'write',
			'openid', 'ou_bot', 'req_legacy_access', 'active', 1, 2, 2
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
	var actionKey, resourceType, resourceToken string
	var supportsAll int
	if err := st.db.QueryRow(
		`SELECT action_key, resource_type, resource_token, supports_all FROM tool_approvals WHERE id='legacy_request'`,
	).Scan(&actionKey, &resourceType, &resourceToken, &supportsAll); err != nil {
		t.Fatalf("query migrated operation approval columns: %v", err)
	}
	if actionKey != "" || resourceType != "" || resourceToken != "" || supportsAll != 0 {
		t.Fatalf("legacy operation scope = %q/%q/%q/%d, want fail-closed empty defaults", actionKey, resourceType, resourceToken, supportsAll)
	}
	var sourceRequestID string
	if err := st.db.QueryRow(`SELECT source_request_id FROM tool_approval_grants`).Scan(&sourceRequestID); err != nil {
		t.Fatalf("query renamed grant source: %v", err)
	}
	if sourceRequestID != "legacy_request" {
		t.Fatalf("source_request_id = %q, want legacy_request", sourceRequestID)
	}
	resource, err := st.GetFeishuBotResource("feishu:cli_test", "folder", "fld_legacy")
	if err != nil || resource.Name != "Legacy Folder" || resource.SourceRequestID != "req_folder" {
		t.Fatalf("backfilled bot resource = %#v err=%v", resource, err)
	}
	capability, active, err := st.ActiveFeishuResourceCapability(
		"feishu:cli_test", "docx", "doxcn_legacy", "openid", "ou_bot", FeishuResourcePermissionRead,
	)
	if err != nil || !active || capability.Permission != FeishuResourcePermissionWrite || capability.SourceRequestID != "req_legacy_access" {
		t.Fatalf("backfilled resource capability = %#v active=%t err=%v", capability, active, err)
	}
	if hasLegacySubject, err := st.tableHasColumn("feishu_resource_grants", "subject_type"); err != nil || hasLegacySubject {
		t.Fatalf("migrated resource grant subject column present=%t err=%v", hasLegacySubject, err)
	}
	if hasActorScope, err := st.tableHasColumn("feishu_resource_grants", "actor_type"); err != nil || !hasActorScope {
		t.Fatalf("migrated resource grant actor column present=%t err=%v", hasActorScope, err)
	}
}
