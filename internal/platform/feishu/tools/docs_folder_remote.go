package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	"lingobridge/internal/store"
)

func (t *docsFolderService) recoverFolderCreate(ctx context.Context, actor Actor, chat ChatContext, requestID string) (string, error) {
	if actor.OpenID == "" && actor.UserID == "" {
		return "", fmt.Errorf("feishu folder recovery requires the requesting user identity")
	}
	operation, err := t.store.GetFeishuRemoteOperation(requestID, t.accountID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuRemoteOperationNotFound) {
			return "", fmt.Errorf("request_id does not identify a recoverable feishu folder creation")
		}
		return "", fmt.Errorf("load feishu folder remote operation: %w", err)
	}
	if operation.OperationKind != store.FeishuRemoteOperationKindFolderCreate || !trustedRemoteOperationMatches(operation, actor, chat) {
		return "", fmt.Errorf("request_id does not identify a folder creation for the current Feishu user and chat")
	}
	parent, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
		ResourceType:  operation.ParentResourceType,
		ResourceToken: operation.ParentResourceToken,
		Permission:    ResourcePermissionWrite,
	})
	if err != nil {
		return "", fmt.Errorf("revalidate recovered folder target access: %w", err)
	}
	return t.continueFolderCreate(ctx, operation, parent, "")
}

func (t *docsFolderService) continueFolderCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource, expectedOwnerID string) (string, error) {
	operation, pendingStatus, err := t.ensureFolderRemoteResult(ctx, operation, parent, expectedOwnerID)
	if err != nil {
		t.updateWorkflowBestEffort(ctx, operation.RequestID, store.WorkflowRequestStateFailed)
		return "", err
	}
	if pendingStatus != "" {
		t.updateWorkflowBestEffort(ctx, operation.RequestID, store.WorkflowRequestStatePartial)
		warning := "飞书可能已经创建了文件夹，但当前无法唯一确认远端结果；LingoBridge 不会再次调用创建 API。"
		retry := fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s，以重新执行保守核验。", folderCreateToolName, operation.RequestID)
		if operation.RemoteResourceToken != "" {
			warning = "文件夹已创建，但本地创建账本尚未记录成功；请勿重新创建。"
			retry = fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s，以修复本地记录。", folderCreateToolName, operation.RequestID)
		}
		return marshalToolOutput(folderCreateOutput{
			Status:            pendingStatus,
			RequestID:         operation.RequestID,
			FolderToken:       operation.RemoteResourceToken,
			Name:              operation.RequestedName,
			URL:               operation.RemoteURL,
			ParentFolderToken: operation.BindingParentToken,
			Default:           operation.SetDefault,
			Shared:            false,
			Warning:           warning,
			Retry:             retry,
		})
	}
	folder, err := t.persistFolderRemoteOperation(operation)
	if err != nil {
		t.updateWorkflowBestEffort(ctx, operation.RequestID, store.WorkflowRequestStatePartial)
		feishuToolsLog.Error(ctx, "persist created feishu folder failed request=%s account=%s chat=%s folder_ref=%s: %v",
			shortToolRequestID(operation.RequestID), operation.AccountID, operation.ChatID, hashString(operation.RemoteResourceToken), err)
		return marshalToolOutput(folderCreateOutput{
			Status:            "partial",
			RequestID:         operation.RequestID,
			FolderToken:       operation.RemoteResourceToken,
			Name:              operation.RequestedName,
			URL:               operation.RemoteURL,
			ParentFolderToken: operation.BindingParentToken,
			Default:           operation.SetDefault,
			Shared:            false,
			Warning:           "文件夹已创建，但本地资源记录尚未完成；请勿重新创建。",
			Retry:             fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s，以修复本地记录。", folderCreateToolName, operation.RequestID),
		})
	}
	return t.retryFolderShare(ctx, Actor{OpenID: operation.ActorOpenID, UserID: operation.ActorUserID}, ChatContext{ChatID: operation.ChatID}, folder.CreateRequestID)
}

func (t *docsFolderService) ensureFolderRemoteResult(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource, expectedOwnerID string) (store.FeishuRemoteOperation, string, error) {
	if err := validateRemoteOperationParentAccess(operation, parent); err != nil {
		return operation, "", fmt.Errorf("validate authorized folder target: %w", err)
	}
	for {
		switch classifyRemoteCreateState(operation.State) {
		case remoteCreateStateActionStart:
			started, claimed, err := t.store.StartFeishuRemoteOperation(operation.RequestID, operation.AccountID, t.currentTime())
			if err != nil {
				return operation, "", fmt.Errorf("start feishu folder remote operation: %w", err)
			}
			operation = started
			if !claimed {
				continue
			}
			return t.callAndRecordFolderCreate(ctx, operation, parent, expectedOwnerID)
		case remoteCreateStateActionReconcile:
			return t.reconcileFolderCreate(ctx, operation, parent, expectedOwnerID)
		case remoteCreateStateActionUseRecordedResult:
			return operation, "", nil
		case remoteCreateStateActionRejectFailed:
			return operation, "", fmt.Errorf("feishu folder creation previously failed before a recoverable result was recorded")
		default:
			return operation, "", fmt.Errorf("unsupported feishu folder remote operation state %q", operation.State)
		}
	}
}

func (t *docsFolderService) callAndRecordFolderCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource, expectedOwnerID string) (store.FeishuRemoteOperation, string, error) {
	req := larkdrive.NewCreateFolderFileReqBuilder().
		Body(larkdrive.NewCreateFolderFileReqBodyBuilder().Name(operation.RequestedName).FolderToken(parent.ResourceToken).Build()).
		Build()
	resp, callErr := t.client.Drive.File.CreateFolder(ctx, req)
	folderToken := ""
	remoteURL := ""
	if resp != nil && resp.Data != nil {
		folderToken = strings.TrimSpace(deref(resp.Data.Token))
		remoteURL = strings.TrimSpace(deref(resp.Data.Url))
	}
	if callErr == nil && resp != nil && resp.Success() && folderToken != "" {
		if remoteURL == "" {
			remoteURL = defaultRemoteCreateURL("folder", folderToken)
		}
		recorded, pendingStatus := recordDefiniteFeishuRemoteCreate(
			ctx, t.store, operation, "folder", folderToken, remoteURL, t.currentTime(),
		)
		return recorded, pendingStatus, nil
	}
	missingData := resp == nil || (resp.Success() && folderToken == "")
	if feishuFolderCreateUncertain(resp, callErr, missingData) {
		marked, err := t.store.MarkFeishuRemoteOperationReconcileRequired(
			operation.RequestID,
			operation.AccountID,
			"uncertain_create_response",
			t.currentTime(),
		)
		if err != nil {
			return operation, "", fmt.Errorf("mark uncertain feishu folder creation: %w", err)
		}
		feishuToolsLog.Warn(ctx, "feishu folder create outcome requires reconciliation account=%s request=%s parent_ref=%s error_type=%T",
			t.accountID, shortToolRequestID(operation.RequestID), hashString(parent.ResourceToken), callErr)
		return t.reconcileFolderCreate(ctx, marked, parent, expectedOwnerID)
	}
	_, _ = t.store.FailFeishuRemoteOperation(operation.RequestID, operation.AccountID, "definite_create_failure", t.currentTime())
	if callErr != nil {
		return operation, "", fmt.Errorf("create feishu application folder: %w", callErr)
	}
	return operation, "", fmt.Errorf("create feishu application folder code=%d msg=%s", resp.Code, resp.Msg)
}

func (t *docsFolderService) reconcileFolderCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource, expectedOwnerID string) (store.FeishuRemoteOperation, string, error) {
	candidate, outcome, err := reconcileFeishuRemoteCreate(ctx, t.client, operation, parent, expectedOwnerID, t.remoteReconcileDelays)
	if err != nil {
		feishuToolsLog.Warn(ctx, "reconcile feishu folder create failed account=%s request=%s parent_ref=%s: %v",
			t.accountID, shortToolRequestID(operation.RequestID), hashString(operation.ParentResourceToken), err)
		return operation, "reconcile_required", nil
	}
	if outcome != "unique_candidate" {
		unknown, markErr := t.store.MarkFeishuRemoteOperationOutcomeUnknown(
			operation.RequestID,
			operation.AccountID,
			outcome,
			t.currentTime(),
		)
		if markErr != nil {
			return operation, "", fmt.Errorf("record unknown feishu folder create outcome: %w", markErr)
		}
		return unknown, "outcome_unknown", nil
	}
	claimable, rejectionCategory, err := reconciledRemoteCreateCandidateClaimable(t.store, operation, candidate)
	if err != nil {
		return operation, "", fmt.Errorf("validate reconciled feishu folder candidate: %w", err)
	}
	if !claimable {
		unknown, markErr := t.store.MarkFeishuRemoteOperationOutcomeUnknown(
			operation.RequestID,
			operation.AccountID,
			rejectionCategory,
			t.currentTime(),
		)
		if markErr != nil {
			return operation, "", fmt.Errorf("record claimed feishu folder candidate: %w", markErr)
		}
		return unknown, "outcome_unknown", nil
	}
	recorded, pendingStatus, err := recordReconciledFeishuRemoteCreate(ctx, t.store, operation, candidate, t.currentTime())
	if err != nil {
		return operation, "", fmt.Errorf("adopt reconciled feishu folder: %w", err)
	}
	if pendingStatus != "" {
		return recorded, pendingStatus, nil
	}
	feishuToolsLog.Info(ctx, "reconciled unique feishu folder create account=%s request=%s folder_ref=%s parent_ref=%s",
		t.accountID, shortToolRequestID(operation.RequestID), hashString(candidate.Token), hashString(operation.ParentResourceToken))
	return recorded, "", nil
}

func (t *docsFolderService) persistFolderRemoteOperation(operation store.FeishuRemoteOperation) (store.FeishuChatFolder, error) {
	if operation.RemoteResourceToken == "" {
		return store.FeishuChatFolder{}, fmt.Errorf("persist feishu folder remote operation: missing folder token")
	}
	claimable, err := remoteCreateResourceClaimable(t.store, operation, "folder", operation.RemoteResourceToken)
	if err != nil {
		return store.FeishuChatFolder{}, err
	}
	if !claimable {
		return store.FeishuChatFolder{}, fmt.Errorf("persist feishu folder remote operation: resource is already claimed by another request")
	}
	createdAt := operation.RemoteResultAt
	if createdAt.IsZero() {
		createdAt = t.currentTime()
	}
	remoteURL := strings.TrimSpace(operation.RemoteURL)
	if remoteURL == "" {
		remoteURL = defaultRemoteCreateURL("folder", operation.RemoteResourceToken)
	}
	if _, err := t.store.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       operation.AccountID,
		ResourceType:    "folder",
		ResourceToken:   operation.RemoteResourceToken,
		ParentToken:     operation.ParentResourceToken,
		Name:            operation.RequestedName,
		URL:             remoteURL,
		SourceRequestID: operation.RequestID,
		CreatedAt:       createdAt,
	}); err != nil {
		return store.FeishuChatFolder{}, fmt.Errorf("record created Feishu folder ownership: %w", err)
	}
	folder, err := t.store.GetFeishuChatFolderByRequest(operation.AccountID, operation.ChatID, operation.RequestID)
	if err == nil {
		if folder.FolderToken != operation.RemoteResourceToken {
			return store.FeishuChatFolder{}, fmt.Errorf("request is already bound to another Feishu folder")
		}
	} else if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
		folder, err = t.store.SaveFeishuChatFolder(store.FeishuChatFolder{
			AccountID:         operation.AccountID,
			ChatID:            operation.ChatID,
			FolderToken:       operation.RemoteResourceToken,
			Name:              operation.RequestedName,
			URL:               remoteURL,
			ParentFolderToken: operation.BindingParentToken,
			Default:           operation.SetDefault,
			ShareMemberType:   operation.ShareMemberType,
			ShareMemberID:     operation.ShareMemberID,
			ShareState:        store.FeishuFolderShareStatePending,
			CreateRequestID:   operation.RequestID,
			CreatedByOpenID:   operation.ActorOpenID,
			CreatedByUserID:   operation.ActorUserID,
			CreatedAt:         createdAt,
		})
		if err != nil {
			return store.FeishuChatFolder{}, fmt.Errorf("record created Feishu folder binding: %w", err)
		}
	} else {
		return store.FeishuChatFolder{}, fmt.Errorf("load created Feishu folder binding: %w", err)
	}
	if _, err := t.store.MarkFeishuRemoteOperationPersisted(operation.RequestID, operation.AccountID, t.currentTime()); err != nil {
		return store.FeishuChatFolder{}, fmt.Errorf("complete feishu folder remote operation persistence: %w", err)
	}
	return folder, nil
}

func feishuFolderCreateUncertain(resp *larkdrive.CreateFolderFileResp, callErr error, missingData bool) bool {
	if callErr != nil || resp == nil || missingData {
		return true
	}
	return feishuCreateResponseUncertain(resp.ApiResp, nil, false)
}
