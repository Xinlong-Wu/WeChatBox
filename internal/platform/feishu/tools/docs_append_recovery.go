package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const docxAppendStartupRecoveryBatchSize = 100

// RecoverDocxAppendOperations resumes append requests that crossed their
// durable mutation boundary before the previous account runtime stopped.
// Services are deduplicated because all Docs adapters share one docsService.
func RecoverDocxAppendOperations(ctx context.Context, registered []tooltypes.Tool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	seen := make(map[*docsService]struct{})
	for _, candidate := range registered {
		tool, ok := candidate.(docsTool)
		if !ok || tool.service == nil {
			continue
		}
		if _, ok := seen[tool.service]; ok {
			continue
		}
		seen[tool.service] = struct{}{}
		if err := tool.service.recoverDocxAppendOperations(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (t *docsService) recoverDocxAppendOperations(ctx context.Context) error {
	if t == nil || t.store == nil || t.appendCipher == nil {
		return nil
	}
	afterCreatedAt := time.Time{}
	afterRequestID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		operations, err := t.store.ListRecoverableFeishuDocxAppendOperationsAfter(
			t.accountID,
			afterCreatedAt,
			afterRequestID,
			docxAppendStartupRecoveryBatchSize,
		)
		if err != nil {
			return fmt.Errorf("list recoverable feishu docx append operations: %w", err)
		}
		for _, operation := range operations {
			if operation.State == store.FeishuDocxAppendOperationStateSucceeded || operation.State == store.FeishuDocxAppendOperationStateFailed {
				t.updateRecoveredDocxAppendWorkflowBestEffort(ctx, operation)
				feishuToolsLog.Info(ctx, "reconciled terminal feishu docx append workflow account=%s request=%s document_ref=%s state=%s",
					operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), operation.State)
				continue
			}
			executionCtx := WithActor(ctx, Actor{OpenID: operation.ActorOpenID, UserID: operation.ActorUserID})
			executionCtx = WithChatContext(executionCtx, ChatContext{ChatID: operation.ChatID})
			authorized, accessErr := requireResourceAccess(executionCtx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
				ResourceType:  "docx",
				ResourceToken: operation.DocumentToken,
				Permission:    ResourcePermissionWrite,
			})
			if accessErr != nil {
				feishuToolsLog.Warn(ctx, "deferred durable feishu docx append recovery because access is unavailable account=%s request=%s document_ref=%s state=%s error_type=%T",
					operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), operation.State, accessErr)
				continue
			}
			if err := validateRecoveredDocxAppendAccess(operation, authorized); err != nil {
				feishuToolsLog.Warn(ctx, "deferred durable feishu docx append recovery because trusted scope mismatched account=%s request=%s document_ref=%s state=%s",
					operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), operation.State)
				continue
			}
			recoveryErr := t.continueDocxAppendOperation(executionCtx, operation, true)
			t.updateRecoveredDocxAppendWorkflowBestEffort(executionCtx, operation)
			if recoveryErr != nil {
				feishuToolsLog.Warn(ctx, "durable feishu docx append recovery incomplete account=%s request=%s document_ref=%s state=%s outcome_unknown=%t error_type=%T",
					operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), operation.State,
					errors.Is(recoveryErr, errDocxAppendOutcomeUnknown), recoveryErr)
				continue
			}
			feishuToolsLog.Info(ctx, "recovered durable feishu docx append account=%s request=%s document_ref=%s",
				operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken))
		}
		if len(operations) < docxAppendStartupRecoveryBatchSize {
			return nil
		}
		last := operations[len(operations)-1]
		afterCreatedAt = last.CreatedAt
		afterRequestID = last.RequestID
	}
}

func validateRecoveredDocxAppendAccess(operation store.FeishuDocxAppendOperation, authorized AuthorizedResource) error {
	if operation.AccountID != authorized.AccountID || operation.ChatID != authorized.ChatID ||
		operation.ActorOpenID != authorized.ActorOpenID || operation.ActorUserID != authorized.ActorUserID ||
		operation.DocumentToken != authorized.ResourceToken ||
		!authorizedResourcePermits(authorized, "docx", operation.DocumentToken, ResourcePermissionWrite) {
		return fmt.Errorf("recovered feishu docx append authorization does not match durable scope")
	}
	return nil
}

func (t *docsService) updateRecoveredDocxAppendWorkflowBestEffort(ctx context.Context, operation store.FeishuDocxAppendOperation) {
	workflow, reconciled, err := t.store.ReconcileFeishuDocxAppendWorkflowState(
		operation.RequestID,
		operation.AccountID,
		t.currentTime(),
	)
	if err != nil {
		feishuToolsLog.Warn(ctx, "reconcile recovered feishu docx append workflow failed account=%s request=%s error_type=%T",
			operation.AccountID, shortToolRequestID(operation.RequestID), err)
		return
	}
	if !reconciled {
		return
	}
	feishuToolsLog.Debug(ctx, "reconciled feishu docx append workflow account=%s request=%s kind=%s state=%s append_state=%s",
		operation.AccountID, shortToolRequestID(operation.RequestID), workflow.Kind, workflow.State, operation.State)
}
