package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	larkdocx "github.com/larksuite/oapi-sdk-go/v3/service/docx/v1"

	"lingobridge/internal/store"
)

func (t *docsService) recoverDocumentCreate(ctx context.Context, actor Actor, chat ChatContext, requestID string) (writeOutput, error) {
	operation, err := t.store.GetFeishuRemoteOperation(requestID, t.accountID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuRemoteOperationNotFound) {
			return writeOutput{}, fmt.Errorf("request_id does not identify a recoverable feishu document creation")
		}
		return writeOutput{}, fmt.Errorf("load feishu document remote operation: %w", err)
	}
	if operation.OperationKind != store.FeishuRemoteOperationKindDocumentCreate || !trustedRemoteOperationMatches(operation, actor, chat) {
		return writeOutput{}, fmt.Errorf("request_id does not identify a document creation for the current Feishu user and chat")
	}
	parent, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
		ResourceType:  operation.ParentResourceType,
		ResourceToken: operation.ParentResourceToken,
		Permission:    ResourcePermissionWrite,
	})
	if err != nil {
		return writeOutput{}, fmt.Errorf("revalidate recovered document target access: %w", err)
	}
	workflow, workflowErr := t.store.GetWorkflowRequest(requestID, t.accountID)
	out, err := t.continueDocumentCreate(ctx, operation, parent, nil)
	if err != nil {
		if out.DocumentID == "" {
			return out, err
		}
		if operation.InitialContentRequested {
			reconciled, managed, reconcileErr := t.store.ReconcileFeishuDocxAppendWorkflowState(requestID, t.accountID, t.currentTime())
			if reconcileErr != nil {
				feishuToolsLog.Warn(ctx, "reconcile recovered feishu document initial-content workflow failed account=%s request=%s error_type=%T",
					t.accountID, shortToolRequestID(requestID), reconcileErr)
			}
			appendOperation, appendErr := t.store.GetFeishuDocxAppendOperation(requestID, t.accountID)
			if appendErr == nil && appendOperation.State == store.FeishuDocxAppendOperationStateSucceeded {
				return out, nil
			}
			if reconcileErr == nil && !managed && reconciled.Kind == store.WorkflowRequestKindFeishuDocsCreate && reconciled.State != store.WorkflowRequestStatePartial {
				t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
			} else if reconcileErr != nil && workflowErr == nil && workflow.Kind == store.WorkflowRequestKindFeishuDocsCreate && workflow.State != store.WorkflowRequestStatePartial {
				t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
			}
		} else if workflowErr == nil && workflow.Kind == store.WorkflowRequestKindFeishuDocsCreate && workflow.State != store.WorkflowRequestStatePartial {
			t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
		}
		if errors.Is(err, errDocxAppendOutcomeUnknown) {
			out.Warning = docxInitialContentOutcomeUnknownMessage(out)
		} else {
			out.Warning = fmt.Sprintf("文档已创建，但初始正文后续处理失败：%v。请勿重复创建，可稍后继续处理。", err)
		}
		return out, nil
	}
	if out.Status == "created" && operation.InitialContentRequested {
		reconciled, managed, reconcileErr := t.store.ReconcileFeishuDocxAppendWorkflowState(requestID, t.accountID, t.currentTime())
		if reconcileErr != nil {
			feishuToolsLog.Warn(ctx, "reconcile recovered feishu document initial-content workflow failed account=%s request=%s error_type=%T",
				t.accountID, shortToolRequestID(requestID), reconcileErr)
		}
		appendOperation, appendErr := t.store.GetFeishuDocxAppendOperation(requestID, t.accountID)
		appendSucceeded := appendErr == nil && appendOperation.State == store.FeishuDocxAppendOperationStateSucceeded
		workflowSucceeded := reconcileErr == nil && managed && reconciled.State == store.WorkflowRequestStateSucceeded
		if !appendSucceeded && !workflowSucceeded {
			out.Warning = "文档创建已恢复，但初始正文是否追加完成无法由创建账本确认；请先检查文档，需要时使用 feishu_docs_append，服务不会猜测并重复追加。"
			if reconcileErr == nil && !managed && reconciled.Kind == store.WorkflowRequestKindFeishuDocsCreate && reconciled.State != store.WorkflowRequestStatePartial {
				t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
			}
		}
	} else if out.Status == "created" {
		if workflowErr == nil && workflow.Kind == store.WorkflowRequestKindFeishuDocsCreate && workflow.State != store.WorkflowRequestStateSucceeded {
			t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStateSucceeded)
		}
	} else if workflowErr == nil && workflow.Kind == store.WorkflowRequestKindFeishuDocsCreate && workflow.State != store.WorkflowRequestStatePartial {
		t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
	}
	return out, nil
}

func (t *docsService) continueDocumentCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource, payload *approvedCreatePayload) (writeOutput, error) {
	operation, pendingStatus, err := t.ensureDocumentRemoteResult(ctx, operation, parent)
	if err != nil {
		return writeOutput{}, err
	}
	if pendingStatus != "" {
		out := documentOutputFromRemoteOperation(operation)
		out.Status = pendingStatus
		if operation.RemoteResourceToken != "" {
			out.Warning = "文档已创建，但本地创建账本尚未记录成功；请勿重新创建。"
			out.Retry = fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s，以修复本地记录。", createToolName, operation.RequestID)
		} else {
			out.Warning = "飞书可能已经创建了文档，但当前无法唯一确认远端结果；LingoBridge 不会再次调用创建 API。"
			out.Retry = fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s，以重新执行保守核验。", createToolName, operation.RequestID)
		}
		return out, nil
	}
	operation, err = t.persistDocumentRemoteOperation(operation)
	if err != nil {
		out := documentOutputFromRemoteOperation(operation)
		out.Status = "partial"
		return out, err
	}
	out := documentOutputFromRemoteOperation(operation)
	out.Status = "created"
	if operation.InitialContentRequested && payload != nil && strings.TrimSpace(payload.Content) != "" {
		authorized, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
			ResourceType:  "docx",
			ResourceToken: operation.RemoteResourceToken,
			Permission:    ResourcePermissionWrite,
		})
		if err != nil {
			return out, fmt.Errorf("authorize initial feishu document content: %w", err)
		}
		if err := t.appendTextBlocks(ctx, operation.RequestID, authorized, payload.Content); err != nil {
			return out, err
		}
	} else if operation.InitialContentRequested && payload == nil {
		appendOperation, err := t.store.GetFeishuDocxAppendOperation(operation.RequestID, operation.AccountID)
		if err != nil {
			if errors.Is(err, store.ErrFeishuDocxAppendOperationNotFound) {
				return out, nil
			}
			return out, fmt.Errorf("load initial feishu document append ledger: %w", err)
		}
		authorized, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
			ResourceType:  "docx",
			ResourceToken: operation.RemoteResourceToken,
			Permission:    ResourcePermissionWrite,
		})
		if err != nil {
			return out, fmt.Errorf("authorize recovered initial feishu document content: %w", err)
		}
		if err := validateRecoveredDocxAppendAccess(appendOperation, authorized); err != nil {
			return out, err
		}
		if err := t.continueDocxAppendOperation(ctx, appendOperation, true); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (t *docsService) ensureDocumentRemoteResult(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource) (store.FeishuRemoteOperation, string, error) {
	if err := validateRemoteOperationParentAccess(operation, parent); err != nil {
		return operation, "", fmt.Errorf("validate authorized document target: %w", err)
	}
	for {
		switch operation.State {
		case store.FeishuRemoteOperationStatePrepared:
			started, claimed, err := t.store.StartFeishuRemoteOperation(operation.RequestID, operation.AccountID, t.currentTime())
			if err != nil {
				return operation, "", fmt.Errorf("start feishu document remote operation: %w", err)
			}
			operation = started
			if !claimed {
				continue
			}
			return t.callAndRecordDocumentCreate(ctx, operation, parent)
		case store.FeishuRemoteOperationStateRemoteStarted,
			store.FeishuRemoteOperationStateReconcileRequired,
			store.FeishuRemoteOperationStateOutcomeUnknown:
			return t.reconcileDocumentCreate(ctx, operation, parent)
		case store.FeishuRemoteOperationStateRemoteSucceeded,
			store.FeishuRemoteOperationStatePersisted:
			return operation, "", nil
		case store.FeishuRemoteOperationStateFailed:
			return operation, "", fmt.Errorf("feishu document creation previously failed before a recoverable result was recorded")
		default:
			return operation, "", fmt.Errorf("unsupported feishu document remote operation state %q", operation.State)
		}
	}
}

func (t *docsService) callAndRecordDocumentCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource) (store.FeishuRemoteOperation, string, error) {
	req := larkdocx.NewCreateDocumentReqBuilder().
		Body(larkdocx.NewCreateDocumentReqBodyBuilder().
			Title(operation.RequestedName).
			FolderToken(parent.ResourceToken).
			Build()).
		Build()
	resp, callErr := t.client.Docx.Document.Create(ctx, req)
	documentID := ""
	if resp != nil && resp.Data != nil && resp.Data.Document != nil {
		documentID = strings.TrimSpace(deref(resp.Data.Document.DocumentId))
	}
	if callErr == nil && resp != nil && resp.Success() && documentID != "" {
		remoteURL := defaultRemoteCreateURL("docx", documentID)
		recorded, pendingStatus := recordDefiniteFeishuRemoteCreate(
			ctx, t.store, operation, "docx", documentID, remoteURL, t.currentTime(),
		)
		return recorded, pendingStatus, nil
	}
	missingData := resp == nil || (resp.Success() && documentID == "")
	if feishuDocumentCreateUncertain(resp, callErr, missingData) {
		marked, err := t.store.MarkFeishuRemoteOperationReconcileRequired(
			operation.RequestID,
			operation.AccountID,
			"uncertain_create_response",
			t.currentTime(),
		)
		if err != nil {
			return operation, "", fmt.Errorf("mark uncertain feishu document creation: %w", err)
		}
		feishuToolsLog.Warn(ctx, "feishu document create outcome requires reconciliation account=%s request=%s folder_ref=%s error_type=%T",
			t.accountID, shortToolRequestID(operation.RequestID), hashString(parent.ResourceToken), callErr)
		return t.reconcileDocumentCreate(ctx, marked, parent)
	}
	_, _ = t.store.FailFeishuRemoteOperation(operation.RequestID, operation.AccountID, "definite_create_failure", t.currentTime())
	if callErr != nil {
		return operation, "", fmt.Errorf("create feishu document: %w", callErr)
	}
	return operation, "", fmt.Errorf("create feishu document code=%d msg=%s", resp.Code, resp.Msg)
}

func (t *docsService) reconcileDocumentCreate(ctx context.Context, operation store.FeishuRemoteOperation, parent AuthorizedResource) (store.FeishuRemoteOperation, string, error) {
	candidate, outcome, err := reconcileFeishuRemoteCreate(ctx, t.client, operation, parent, "", t.remoteReconcileDelays)
	if err != nil {
		feishuToolsLog.Warn(ctx, "reconcile feishu document create failed account=%s request=%s folder_ref=%s: %v",
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
			return operation, "", fmt.Errorf("record unknown feishu document create outcome: %w", markErr)
		}
		return unknown, "outcome_unknown", nil
	}
	claimable, rejectionCategory, err := reconciledRemoteCreateCandidateClaimable(t.store, operation, candidate)
	if err != nil {
		return operation, "", fmt.Errorf("validate reconciled feishu document candidate: %w", err)
	}
	if !claimable {
		unknown, markErr := t.store.MarkFeishuRemoteOperationOutcomeUnknown(
			operation.RequestID,
			operation.AccountID,
			rejectionCategory,
			t.currentTime(),
		)
		if markErr != nil {
			return operation, "", fmt.Errorf("record claimed feishu document candidate: %w", markErr)
		}
		return unknown, "outcome_unknown", nil
	}
	recorded, pendingStatus, err := recordReconciledFeishuRemoteCreate(ctx, t.store, operation, candidate, t.currentTime())
	if err != nil {
		return operation, "", fmt.Errorf("adopt reconciled feishu document: %w", err)
	}
	if pendingStatus != "" {
		return recorded, pendingStatus, nil
	}
	feishuToolsLog.Info(ctx, "reconciled unique feishu document create account=%s request=%s document_ref=%s folder_ref=%s",
		t.accountID, shortToolRequestID(operation.RequestID), hashString(candidate.Token), hashString(operation.ParentResourceToken))
	return recorded, "", nil
}

func (t *docsService) persistDocumentRemoteOperation(operation store.FeishuRemoteOperation) (store.FeishuRemoteOperation, error) {
	if operation.RemoteResourceToken == "" {
		return operation, fmt.Errorf("persist feishu document remote operation: missing document token")
	}
	claimable, err := remoteCreateResourceClaimable(t.store, operation, "docx", operation.RemoteResourceToken)
	if err != nil {
		return operation, err
	}
	if !claimable {
		return operation, fmt.Errorf("persist feishu document remote operation: resource is already claimed by another request")
	}
	createdAt := operation.RemoteResultAt
	if createdAt.IsZero() {
		createdAt = t.currentTime()
	}
	remoteURL := strings.TrimSpace(operation.RemoteURL)
	if remoteURL == "" {
		remoteURL = defaultRemoteCreateURL("docx", operation.RemoteResourceToken)
	}
	if _, err := t.store.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       operation.AccountID,
		ResourceType:    "docx",
		ResourceToken:   operation.RemoteResourceToken,
		ParentToken:     operation.ParentResourceToken,
		Name:            operation.RequestedName,
		URL:             remoteURL,
		SourceRequestID: operation.RequestID,
		CreatedAt:       createdAt,
	}); err != nil {
		return operation, fmt.Errorf("record created Feishu document ownership: %w", err)
	}
	existing, err := t.store.GetFeishuChatDocumentByRequest(operation.AccountID, operation.ChatID, operation.RequestID)
	if err == nil && existing.DocumentToken != operation.RemoteResourceToken {
		return operation, fmt.Errorf("record created Feishu document: request is already bound to another document")
	}
	if err != nil && !errors.Is(err, store.ErrFeishuChatDocumentNotFound) {
		return operation, fmt.Errorf("load created Feishu document binding: %w", err)
	}
	if _, err := t.store.SaveFeishuChatDocument(store.FeishuChatDocument{
		AccountID:       operation.AccountID,
		ChatID:          operation.ChatID,
		DocumentToken:   operation.RemoteResourceToken,
		FolderToken:     operation.BindingParentToken,
		Title:           operation.RequestedName,
		URL:             remoteURL,
		SourceRequestID: operation.RequestID,
		CreatedAt:       createdAt,
	}); err != nil {
		return operation, fmt.Errorf("record created Feishu document: %w", err)
	}
	persisted, err := t.store.MarkFeishuRemoteOperationPersisted(operation.RequestID, operation.AccountID, t.currentTime())
	if err != nil {
		return operation, fmt.Errorf("complete feishu document remote operation persistence: %w", err)
	}
	return persisted, nil
}

func documentOutputFromRemoteOperation(operation store.FeishuRemoteOperation) writeOutput {
	remoteURL := strings.TrimSpace(operation.RemoteURL)
	if remoteURL == "" && operation.RemoteResourceToken != "" {
		remoteURL = defaultRemoteCreateURL("docx", operation.RemoteResourceToken)
	}
	return writeOutput{
		RequestID:  operation.RequestID,
		DocumentID: operation.RemoteResourceToken,
		Title:      operation.RequestedName,
		URL:        remoteURL,
	}
}

func feishuDocumentCreateUncertain(resp *larkdocx.CreateDocumentResp, callErr error, missingData bool) bool {
	if callErr != nil || resp == nil || missingData {
		return true
	}
	return feishuCreateResponseUncertain(resp.ApiResp, nil, false)
}
