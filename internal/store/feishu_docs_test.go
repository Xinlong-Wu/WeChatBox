package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFeishuChatFoldersAreChatScopedWithOneDefault(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	first, err := st.SaveFeishuChatFolder(FeishuChatFolder{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		FolderToken:     "fld_first",
		Name:            "First",
		ShareMemberType: "openchat",
		ShareMemberID:   "oc_chat",
		ShareState:      FeishuFolderShareStatePending,
		CreateRequestID: "req_first",
		CreatedByOpenID: "ou_requester",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatFolder first returned error: %v", err)
	}
	if !first.Default {
		t.Fatalf("first folder = %#v, want automatic default", first)
	}
	second, err := st.SaveFeishuChatFolder(FeishuChatFolder{
		AccountID:         first.AccountID,
		ChatID:            first.ChatID,
		FolderToken:       "fld_second",
		Name:              "Second",
		ParentFolderToken: first.FolderToken,
		ShareMemberType:   first.ShareMemberType,
		ShareMemberID:     first.ShareMemberID,
		ShareState:        FeishuFolderShareStateSucceeded,
		CreateRequestID:   "req_second",
		CreatedByOpenID:   first.CreatedByOpenID,
		CreatedAt:         now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatFolder second returned error: %v", err)
	}
	if second.Default {
		t.Fatalf("second folder = %#v, want existing default retained", second)
	}
	third, err := st.SaveFeishuChatFolder(FeishuChatFolder{
		AccountID:       first.AccountID,
		ChatID:          first.ChatID,
		FolderToken:     "fld_third",
		Name:            "Third",
		Default:         true,
		ShareMemberType: first.ShareMemberType,
		ShareMemberID:   first.ShareMemberID,
		ShareState:      FeishuFolderShareStateSucceeded,
		CreateRequestID: "req_third",
		CreatedByOpenID: first.CreatedByOpenID,
		CreatedAt:       now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatFolder third returned error: %v", err)
	}
	defaultFolder, err := st.DefaultFeishuChatFolder(first.AccountID, first.ChatID)
	if err != nil || defaultFolder.FolderToken != third.FolderToken {
		t.Fatalf("default folder = %#v err=%v", defaultFolder, err)
	}
	folders, err := st.ListFeishuChatFolders(first.AccountID, first.ChatID)
	if err != nil {
		t.Fatalf("ListFeishuChatFolders returned error: %v", err)
	}
	if len(folders) != 3 || folders[0].FolderToken != third.FolderToken || !folders[0].Default {
		t.Fatalf("folders = %#v, want default first", folders)
	}
	if _, err := st.GetFeishuChatFolder(first.AccountID, "oc_other", first.FolderToken); !errors.Is(err, ErrFeishuChatFolderNotFound) {
		t.Fatalf("cross-chat folder error = %v, want ErrFeishuChatFolderNotFound", err)
	}
	byRequest, err := st.GetFeishuChatFolderByRequest(first.AccountID, first.ChatID, second.CreateRequestID)
	if err != nil || byRequest.FolderToken != second.FolderToken {
		t.Fatalf("folder by request = %#v err=%v", byRequest, err)
	}
}

func TestFeishuFolderShareStateAndDocumentsAreDurableAndScoped(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	folder, err := st.SaveFeishuChatFolder(FeishuChatFolder{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		FolderToken:     "fld_token",
		Name:            "Docs",
		ShareMemberType: "openid",
		ShareMemberID:   "ou_requester",
		ShareState:      FeishuFolderShareStatePending,
		CreateRequestID: "req_folder",
		CreatedByOpenID: "ou_requester",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
	}
	if err := st.UpdateFeishuChatFolderShareState(folder.AccountID, folder.ChatID, folder.FolderToken, FeishuFolderShareStateFailed, now.Add(time.Second)); err != nil {
		t.Fatalf("UpdateFeishuChatFolderShareState returned error: %v", err)
	}
	updated, err := st.GetFeishuChatFolder(folder.AccountID, folder.ChatID, folder.FolderToken)
	if err != nil || updated.ShareState != FeishuFolderShareStateFailed || !updated.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("updated folder = %#v err=%v", updated, err)
	}
	document, err := st.SaveFeishuChatDocument(FeishuChatDocument{
		AccountID:       folder.AccountID,
		ChatID:          folder.ChatID,
		DocumentToken:   "doxcn_document",
		FolderToken:     folder.FolderToken,
		Title:           "Plan",
		URL:             "https://docs.feishu.cn/docx/doxcn_document",
		SourceRequestID: "req_document",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuChatDocument returned error: %v", err)
	}
	got, err := st.GetFeishuChatDocument(document.AccountID, document.ChatID, document.DocumentToken)
	if err != nil || got.FolderToken != folder.FolderToken || got.SourceRequestID != document.SourceRequestID {
		t.Fatalf("document = %#v err=%v", got, err)
	}
	if _, err := st.GetFeishuChatDocument(document.AccountID, "oc_other", document.DocumentToken); !errors.Is(err, ErrFeishuChatDocumentNotFound) {
		t.Fatalf("cross-chat document error = %v, want ErrFeishuChatDocumentNotFound", err)
	}
}

func TestDeleteFeishuDocsDataRemovesOnlyMatchingAccount(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	accessIDs := map[string]string{}
	refreshAttemptIDs := map[string]string{}
	remoteOperationIDs := map[string]string{}
	for _, accountID := range []string{"feishu:first", "feishu:second"} {
		if _, err := st.AcquireFeishuAccountRuntimeLease(accountID, "runtime-"+accountID, now, time.Minute); err != nil {
			t.Fatalf("AcquireFeishuAccountRuntimeLease returned error: %v", err)
		}
		if _, err := st.EnqueueFeishuCardDelivery(FeishuCardDelivery{
			AccountID:     accountID,
			RequestID:     "req-card-" + accountID,
			Purpose:       FeishuCardDeliveryPurposeResourceTerminal,
			Revision:      FeishuCardDeliveryRevisionTerminal,
			CardMessageID: "om-card-" + accountID,
			CreatedAt:     now,
			ExpiresAt:     now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("EnqueueFeishuCardDelivery returned error: %v", err)
		}
		request, err := st.CreateWorkflowRequest(WorkflowRequest{
			AccountID: accountID,
			Kind:      WorkflowRequestKindFeishuFolderCreate,
			State:     WorkflowRequestStateSucceeded,
			CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateWorkflowRequest returned error: %v", err)
		}
		folder, err := st.SaveFeishuChatFolder(FeishuChatFolder{
			AccountID:       accountID,
			ChatID:          "oc_chat",
			FolderToken:     "fld_" + accountID,
			Name:            "Docs",
			ShareMemberType: "openchat",
			ShareMemberID:   "oc_chat",
			ShareState:      FeishuFolderShareStateSucceeded,
			CreateRequestID: request.ID,
			CreatedAt:       now,
		})
		if err != nil {
			t.Fatalf("SaveFeishuChatFolder returned error: %v", err)
		}
		if _, err := st.PrepareFeishuRemoteOperation(FeishuRemoteOperation{
			RequestID:           request.ID,
			AccountID:           accountID,
			OperationKind:       FeishuRemoteOperationKindFolderCreate,
			ChatID:              folder.ChatID,
			ActorOpenID:         "ou_requester",
			ParentResourceType:  "folder",
			ParentResourceToken: "fld_root_" + accountID,
			RequestedName:       folder.Name,
			PayloadHash:         "payload_" + accountID,
			ShareMemberType:     folder.ShareMemberType,
			ShareMemberID:       folder.ShareMemberID,
			RemoteResourceType:  "folder",
			CreatedAt:           now,
		}); err != nil {
			t.Fatalf("PrepareFeishuRemoteOperation returned error: %v", err)
		}
		remoteOperationIDs[accountID] = request.ID
		if _, err := st.SaveFeishuChatDocument(FeishuChatDocument{
			AccountID:     accountID,
			ChatID:        folder.ChatID,
			DocumentToken: "doc_" + accountID,
			FolderToken:   folder.FolderToken,
			CreatedAt:     now,
		}); err != nil {
			t.Fatalf("SaveFeishuChatDocument returned error: %v", err)
		}
		if _, err := st.SaveFeishuBotResource(FeishuBotResource{
			AccountID:       accountID,
			ResourceType:    "folder",
			ResourceToken:   folder.FolderToken,
			Name:            folder.Name,
			SourceRequestID: request.ID,
			CreatedAt:       now,
		}); err != nil {
			t.Fatalf("SaveFeishuBotResource returned error: %v", err)
		}
		access, err := st.CreateFeishuResourceAccessRequest(FeishuResourceAccessRequest{
			AccountID:           accountID,
			ActorOpenID:         "ou_requester",
			ChatID:              folder.ChatID,
			ResourceType:        "folder",
			ResourceToken:       folder.FolderToken,
			Permission:          FeishuResourcePermissionWrite,
			OnceDurationMinutes: 30,
			CreatedAt:           now,
			ExpiresAt:           now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
		}
		if err := st.CompleteFeishuResourceAccessRequest(
			access.ID,
			accountID,
			FeishuResourceGrantSourceBotOwner,
			FeishuResourcePermissionWrite,
			nil,
			nil,
			now,
		); err != nil {
			t.Fatalf("CompleteFeishuResourceAccessRequest returned error: %v", err)
		}
		if _, err := st.UpsertFeishuResourceCapability(FeishuResourceCapability{
			AccountID:         accountID,
			ResourceType:      "folder",
			ResourceToken:     folder.FolderToken,
			SubjectType:       "openchat",
			SubjectID:         folder.ChatID,
			Permission:        FeishuResourcePermissionWrite,
			SourceActorOpenID: "ou_requester",
			SourceRequestID:   access.ID,
			State:             FeishuResourceCapabilityStateActive,
			CreatedAt:         now,
			VerifiedAt:        now,
		}); err != nil {
			t.Fatalf("UpsertFeishuResourceCapability returned error: %v", err)
		}
		credential, err := st.SaveFeishuUserOAuthCredential(FeishuUserOAuthCredential{
			AccountID:              accountID,
			ActorOpenID:            "ou_requester",
			ActorUserID:            "u_requester",
			AccessTokenCiphertext:  "v1.access-" + accountID,
			AccessTokenExpiresAt:   now.Add(2 * time.Hour),
			RefreshTokenCiphertext: "v1.refresh-" + accountID,
			RefreshTokenExpiresAt:  now.Add(30 * 24 * time.Hour),
			Scopes:                 "auth:user.id:read docs:permission.member:create offline_access",
			AuthorizedAt:           now,
			ReauthorizeAt:          now.Add(365 * 24 * time.Hour),
			Status:                 FeishuUserOAuthCredentialStatusActive,
			CreatedAt:              now,
			UpdatedAt:              now,
		})
		if err != nil {
			t.Fatalf("SaveFeishuUserOAuthCredential returned error: %v", err)
		}
		refreshAttempt, _, err := st.PrepareFeishuOAuthRefreshAttempt(
			credential.ID,
			credential.AccountID,
			credential.Version,
			"lease-"+accountID,
			now,
			time.Minute,
		)
		if err != nil {
			t.Fatalf("PrepareFeishuOAuthRefreshAttempt returned error: %v", err)
		}
		refreshAttemptIDs[accountID] = refreshAttempt.ID
		continuation := attachWorkflowContinuationForTest(t, st, access.ID, accountID, now, 0)
		if _, _, err := st.CommitWorkflowContinuation(continuation.RequestID, continuation.AccountID, 1, now.Add(time.Second)); err != nil {
			t.Fatalf("CommitWorkflowContinuation returned error: %v", err)
		}
		if _, _, _, err := st.StoreWorkflowResult(WorkflowResult{
			RequestID: continuation.RequestID,
			AccountID: continuation.AccountID,
			State:     WorkflowResultStateSucceeded,
			CreatedAt: now.Add(2 * time.Second),
		}); err != nil {
			t.Fatalf("StoreWorkflowResult returned error: %v", err)
		}
		accessIDs[accountID] = access.ID
	}
	if err := st.DeleteFeishuDocsData("feishu:first"); err != nil {
		t.Fatalf("DeleteFeishuDocsData returned error: %v", err)
	}
	if folders, err := st.ListFeishuChatFolders("feishu:first", "oc_chat"); err != nil || len(folders) != 0 {
		t.Fatalf("deleted account folders = %#v err=%v", folders, err)
	}
	if folders, err := st.ListFeishuChatFolders("feishu:second", "oc_chat"); err != nil || len(folders) != 1 {
		t.Fatalf("other account folders = %#v err=%v", folders, err)
	}
	if _, err := st.GetFeishuBotResource("feishu:first", "folder", "fld_feishu:first"); !errors.Is(err, ErrFeishuBotResourceNotFound) {
		t.Fatalf("deleted account bot resource error = %v, want ErrFeishuBotResourceNotFound", err)
	}
	if _, err := st.GetFeishuBotResource("feishu:second", "folder", "fld_feishu:second"); err != nil {
		t.Fatalf("other account bot resource error = %v", err)
	}
	if _, err := st.GetFeishuUserOAuthCredential("feishu:first", "ou_requester", "u_requester"); !errors.Is(err, ErrFeishuUserOAuthCredentialNotFound) {
		t.Fatalf("deleted account OAuth credential error = %v, want ErrFeishuUserOAuthCredentialNotFound", err)
	}
	if _, err := st.GetFeishuUserOAuthCredential("feishu:second", "ou_requester", "u_requester"); err != nil {
		t.Fatalf("other account OAuth credential was deleted: %v", err)
	}
	if _, err := st.GetFeishuOAuthRefreshAttempt(refreshAttemptIDs["feishu:first"], "feishu:first"); !errors.Is(err, ErrFeishuOAuthRefreshAttemptNotFound) {
		t.Fatalf("deleted account refresh attempt error = %v, want ErrFeishuOAuthRefreshAttemptNotFound", err)
	}
	if _, err := st.GetFeishuOAuthRefreshAttempt(refreshAttemptIDs["feishu:second"], "feishu:second"); err != nil {
		t.Fatalf("other account OAuth refresh attempt was deleted: %v", err)
	}
	if _, err := st.GetFeishuAccountRuntimeLease("feishu:first"); !errors.Is(err, ErrFeishuAccountRuntimeLeaseNotFound) {
		t.Fatalf("deleted account runtime lease error = %v, want ErrFeishuAccountRuntimeLeaseNotFound", err)
	}
	if lease, err := st.GetFeishuAccountRuntimeLease("feishu:second"); err != nil || lease.OwnerID != "runtime-feishu:second" {
		t.Fatalf("other account runtime lease = %#v err=%v", lease, err)
	}
	if _, err := st.GetFeishuCardDeliveryByKey(
		"feishu:first", "req-card-feishu:first", FeishuCardDeliveryPurposeResourceTerminal, FeishuCardDeliveryRevisionTerminal,
	); !errors.Is(err, ErrFeishuCardDeliveryNotFound) {
		t.Fatalf("deleted account card delivery error = %v, want ErrFeishuCardDeliveryNotFound", err)
	}
	if _, err := st.GetFeishuCardDeliveryByKey(
		"feishu:second", "req-card-feishu:second", FeishuCardDeliveryPurposeResourceTerminal, FeishuCardDeliveryRevisionTerminal,
	); err != nil {
		t.Fatalf("other account card delivery was deleted: %v", err)
	}
	if _, err := st.GetFeishuRemoteOperation(remoteOperationIDs["feishu:first"], "feishu:first"); !errors.Is(err, ErrFeishuRemoteOperationNotFound) {
		t.Fatalf("deleted account remote operation error = %v, want ErrFeishuRemoteOperationNotFound", err)
	}
	if _, err := st.GetFeishuRemoteOperation(remoteOperationIDs["feishu:second"], "feishu:second"); err != nil {
		t.Fatalf("other account remote operation was deleted: %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		"feishu:first", "folder", "fld_feishu:first", "openchat", "oc_chat", FeishuResourcePermissionRead,
	); err != nil || active {
		t.Fatalf("deleted account capability active=%t err=%v, want false", active, err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		"feishu:second", "folder", "fld_feishu:second", "openchat", "oc_chat", FeishuResourcePermissionRead,
	); err != nil || !active {
		t.Fatalf("other account capability active=%t err=%v, want true", active, err)
	}
	var deletedAccessID string
	if err := st.db.QueryRow(
		`SELECT id FROM workflow_requests WHERE account_id=? AND kind=?`,
		"feishu:first", WorkflowRequestKindFeishuResourceAccess,
	).Scan(&deletedAccessID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted account resource workflow query error = %v, want sql.ErrNoRows", err)
	}
	var otherAccessID string
	if err := st.db.QueryRow(
		`SELECT id FROM workflow_requests WHERE account_id=? AND kind=?`,
		"feishu:second", WorkflowRequestKindFeishuResourceAccess,
	).Scan(&otherAccessID); err != nil || otherAccessID == "" {
		t.Fatalf("other account resource workflow id = %q err=%v", otherAccessID, err)
	}
	if _, err := st.GetWorkflowContinuation(accessIDs["feishu:first"], "feishu:first"); !errors.Is(err, ErrWorkflowContinuationNotFound) {
		t.Fatalf("deleted account continuation error = %v, want ErrWorkflowContinuationNotFound", err)
	}
	if _, err := st.GetWorkflowResult(accessIDs["feishu:first"], "feishu:first"); !errors.Is(err, ErrWorkflowResultNotFound) {
		t.Fatalf("deleted account workflow result error = %v, want ErrWorkflowResultNotFound", err)
	}
	if _, err := st.GetWorkflowContinuation(accessIDs["feishu:second"], "feishu:second"); err != nil {
		t.Fatalf("other account workflow continuation was deleted: %v", err)
	}
	if _, err := st.GetWorkflowResult(accessIDs["feishu:second"], "feishu:second"); err != nil {
		t.Fatalf("other account workflow result was deleted: %v", err)
	}
}

func openFeishuDocsTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	st, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return st
}
