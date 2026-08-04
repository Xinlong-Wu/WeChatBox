package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	larkdocx "github.com/larksuite/oapi-sdk-go/v3/service/docx/v1"

	feishuidempotency "lingobridge/internal/platform/feishu/idempotency"
	"lingobridge/internal/store"
)

const docxAppendExecutionLease = time.Minute

type docxAppendEnvelope struct {
	DocumentToken string            `json:"document_token"`
	ClientToken   string            `json:"client_token"`
	Index         int               `json:"index"`
	Children      []*larkdocx.Block `json:"children"`
}

type docxAppendIntent struct {
	AccountID     string            `json:"account_id"`
	ChatID        string            `json:"chat_id"`
	ActorOpenID   string            `json:"actor_open_id,omitempty"`
	ActorUserID   string            `json:"actor_user_id,omitempty"`
	DocumentToken string            `json:"document_token"`
	Children      []*larkdocx.Block `json:"children"`
}

func (t *docsService) executeDocxAppend(ctx context.Context, requestID string, document AuthorizedResource, content string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	blocks := textBlocks(content)
	if len(blocks) == 0 {
		return nil
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Errorf("append feishu document: stable workflow request id is required")
	}
	documentID := strings.TrimSpace(document.ResourceToken)
	if documentID == "" || !authorizedResourcePermits(document, "docx", documentID, ResourcePermissionWrite) {
		return fmt.Errorf("append feishu document: authorized docx/write resource is required")
	}

	// Low-level unit tests may construct a service without persistence. Every
	// production service is built through NewDocsTools and has both dependencies.
	if t.store == nil && t.appendCipher == nil {
		index, err := t.docxTopLevelChildCount(ctx, document)
		if err != nil {
			return err
		}
		return t.callFrozenDocxAppend(ctx, store.FeishuDocxAppendOperation{
			RequestID:     requestID,
			AccountID:     t.accountID,
			DocumentToken: documentID,
		}, docxAppendEnvelope{
			DocumentToken: documentID,
			ClientToken:   docxAppendClientToken(requestID),
			Index:         index,
			Children:      blocks,
		}, false, "", false)
	}
	if t.store == nil || t.appendCipher == nil || strings.TrimSpace(t.appendExecutionOwner) == "" {
		return fmt.Errorf("append feishu document: durable ledger, envelope cipher, and runtime execution owner are required")
	}

	operation, payloadHash, err := t.docxAppendOperationIdentity(requestID, document, blocks)
	if err != nil {
		return err
	}
	existing, err := t.store.GetFeishuDocxAppendOperation(requestID, t.accountID)
	if err == nil {
		if err := validateDocxAppendReplay(existing, operation, payloadHash); err != nil {
			return err
		}
		return t.continueDocxAppendOperation(ctx, existing, true)
	}
	if !errors.Is(err, store.ErrFeishuDocxAppendOperationNotFound) {
		return fmt.Errorf("load feishu docx append ledger: %w", err)
	}

	index, err := t.docxTopLevelChildCount(ctx, document)
	if err != nil {
		return err
	}
	envelope := docxAppendEnvelope{
		DocumentToken: operation.DocumentToken,
		ClientToken:   operation.ClientToken,
		Index:         index,
		Children:      blocks,
	}
	envelopeRaw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal feishu docx append envelope: %w", err)
	}
	operation.InsertionIndex = index
	operation.EnvelopeHash, err = remoteOperationPayloadHash(envelope)
	if err != nil {
		return err
	}
	operation.EnvelopeCiphertext, err = t.appendCipher.encrypt(operation, envelopeRaw)
	if err != nil {
		return err
	}
	prepared, created, err := t.store.PrepareFeishuDocxAppendOperation(operation)
	if err != nil {
		return fmt.Errorf("prepare feishu docx append ledger: %w", err)
	}
	if !created {
		if err := validateDocxAppendReplay(prepared, operation, payloadHash); err != nil {
			return err
		}
		if prepared.State == store.FeishuDocxAppendOperationStateRemoteStarted {
			return fmt.Errorf("%w while another caller owns the first append attempt", errDocxAppendOutcomeUnknown)
		}
	}
	feishuToolsLog.Debug(ctx, "prepared durable feishu docx append account=%s request=%s document_ref=%s state=%s payload_ref=%s envelope_ref=%s created=%t",
		t.accountID, shortToolRequestID(requestID), hashString(documentID), prepared.State,
		shortHash(payloadHash), shortHash(prepared.EnvelopeHash), created)
	return t.continueDocxAppendOperation(ctx, prepared, !created)
}

func (t *docsService) docxAppendOperationIdentity(requestID string, document AuthorizedResource, blocks []*larkdocx.Block) (store.FeishuDocxAppendOperation, string, error) {
	accountID := strings.TrimSpace(t.accountID)
	if accountID == "" || strings.TrimSpace(document.AccountID) != accountID || strings.TrimSpace(document.ChatID) == "" ||
		(strings.TrimSpace(document.ActorOpenID) == "" && strings.TrimSpace(document.ActorUserID) == "") {
		return store.FeishuDocxAppendOperation{}, "", fmt.Errorf("append feishu document: trusted account, chat, and actor scope are required")
	}
	intent := docxAppendIntent{
		AccountID:     accountID,
		ChatID:        strings.TrimSpace(document.ChatID),
		ActorOpenID:   strings.TrimSpace(document.ActorOpenID),
		ActorUserID:   strings.TrimSpace(document.ActorUserID),
		DocumentToken: strings.TrimSpace(document.ResourceToken),
		Children:      blocks,
	}
	payloadHash, err := remoteOperationPayloadHash(intent)
	if err != nil {
		return store.FeishuDocxAppendOperation{}, "", err
	}
	return store.FeishuDocxAppendOperation{
		RequestID:     strings.TrimSpace(requestID),
		AccountID:     accountID,
		ChatID:        intent.ChatID,
		ActorOpenID:   intent.ActorOpenID,
		ActorUserID:   intent.ActorUserID,
		DocumentToken: intent.DocumentToken,
		ClientToken:   docxAppendClientToken(requestID),
		PayloadHash:   payloadHash,
		CreatedAt:     t.currentTime(),
	}, payloadHash, nil
}

func validateDocxAppendReplay(existing, candidate store.FeishuDocxAppendOperation, payloadHash string) error {
	if existing.RequestID != candidate.RequestID || existing.AccountID != candidate.AccountID ||
		existing.ChatID != candidate.ChatID || existing.ActorOpenID != candidate.ActorOpenID ||
		existing.ActorUserID != candidate.ActorUserID || existing.DocumentToken != candidate.DocumentToken ||
		existing.ClientToken != candidate.ClientToken || existing.PayloadHash != payloadHash {
		return store.ErrFeishuDocxAppendOperationConflict
	}
	return nil
}

func (t *docsService) continueDocxAppendOperation(ctx context.Context, operation store.FeishuDocxAppendOperation, allowRecovery bool) error {
	for {
		switch operation.State {
		case store.FeishuDocxAppendOperationStatePrepared:
			envelope, err := t.decryptDocxAppendEnvelope(operation)
			if err != nil {
				return err
			}
			executionToken, err := newDocxAppendExecutionToken()
			if err != nil {
				return err
			}
			started, claimed, err := t.store.StartFeishuDocxAppendOperation(
				operation.RequestID,
				operation.AccountID,
				t.appendExecutionOwner,
				executionToken,
				t.currentTime(),
				docxAppendExecutionLease,
			)
			if err != nil {
				return fmt.Errorf("start feishu docx append ledger: %w", err)
			}
			operation = started
			if !claimed {
				return docxAppendUnclaimedResult(operation, "another caller owns the first append attempt")
			}
			feishuToolsLog.Info(ctx, "claimed first durable feishu docx append account=%s request=%s document_ref=%s payload_ref=%s envelope_ref=%s",
				operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken),
				shortHash(operation.PayloadHash), shortHash(operation.EnvelopeHash))
			return t.callFrozenDocxAppend(ctx, operation, envelope, true, executionToken, false)
		case store.FeishuDocxAppendOperationStateRemoteStarted,
			store.FeishuDocxAppendOperationStateOutcomeUnknown:
			if !allowRecovery {
				return fmt.Errorf("%w while another caller owns the first append attempt", errDocxAppendOutcomeUnknown)
			}
			envelope, err := t.decryptDocxAppendEnvelope(operation)
			if err != nil {
				return err
			}
			executionToken, err := newDocxAppendExecutionToken()
			if err != nil {
				return err
			}
			recovered, claimed, err := t.store.ClaimFeishuDocxAppendOperationRecovery(
				operation.RequestID,
				operation.AccountID,
				t.appendExecutionOwner,
				executionToken,
				t.currentTime(),
				docxAppendExecutionLease,
			)
			if err != nil {
				return fmt.Errorf("claim feishu docx append recovery: %w", err)
			}
			operation = recovered
			if !claimed {
				return docxAppendUnclaimedResult(operation, "another caller owns append recovery")
			}
			feishuToolsLog.Info(ctx, "reusing frozen durable feishu docx append account=%s request=%s document_ref=%s state=%s payload_ref=%s envelope_ref=%s",
				operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), operation.State,
				shortHash(operation.PayloadHash), shortHash(operation.EnvelopeHash))
			return t.callFrozenDocxAppend(ctx, operation, envelope, true, executionToken, true)
		case store.FeishuDocxAppendOperationStateSucceeded:
			return nil
		case store.FeishuDocxAppendOperationStateFailed:
			return fmt.Errorf("feishu document append previously failed: %s", operation.LastErrorCategory)
		default:
			return fmt.Errorf("unsupported feishu docx append operation state %q", operation.State)
		}
	}
}

func docxAppendUnclaimedResult(operation store.FeishuDocxAppendOperation, reason string) error {
	switch operation.State {
	case store.FeishuDocxAppendOperationStateSucceeded:
		return nil
	case store.FeishuDocxAppendOperationStateFailed:
		return fmt.Errorf("feishu document append previously failed: %s", operation.LastErrorCategory)
	default:
		return fmt.Errorf("%w while %s", errDocxAppendOutcomeUnknown, reason)
	}
}

func (t *docsService) decryptDocxAppendEnvelope(operation store.FeishuDocxAppendOperation) (docxAppendEnvelope, error) {
	if strings.TrimSpace(operation.EnvelopeCiphertext) == "" {
		return docxAppendEnvelope{}, fmt.Errorf("recover feishu docx append: protected envelope is unavailable")
	}
	raw, err := t.appendCipher.decrypt(operation)
	if err != nil {
		return docxAppendEnvelope{}, err
	}
	var envelope docxAppendEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return docxAppendEnvelope{}, fmt.Errorf("decode feishu docx append envelope: %w", err)
	}
	hash, err := remoteOperationPayloadHash(envelope)
	if err != nil {
		return docxAppendEnvelope{}, err
	}
	if envelope.DocumentToken != operation.DocumentToken || envelope.ClientToken != operation.ClientToken || envelope.Index != operation.InsertionIndex ||
		len(envelope.Children) == 0 || hash != operation.EnvelopeHash {
		return docxAppendEnvelope{}, fmt.Errorf("recover feishu docx append: protected envelope does not match durable metadata")
	}
	return envelope, nil
}

func (t *docsService) callFrozenDocxAppend(ctx context.Context, operation store.FeishuDocxAppendOperation, envelope docxAppendEnvelope, durable bool, executionToken string, recovery bool) error {
	documentID := strings.TrimSpace(envelope.DocumentToken)
	requestID := strings.TrimSpace(operation.RequestID)
	if documentID == "" || requestID == "" || strings.TrimSpace(envelope.ClientToken) == "" {
		return fmt.Errorf("append feishu document: frozen operation metadata is incomplete")
	}
	if durable && documentID != strings.TrimSpace(operation.DocumentToken) {
		return fmt.Errorf("append feishu document: frozen document does not match durable metadata")
	}
	if durable && strings.TrimSpace(executionToken) == "" {
		return fmt.Errorf("append feishu document: durable execution token is required")
	}
	req := larkdocx.NewCreateDocumentBlockChildrenReqBuilder().
		DocumentId(documentID).
		BlockId(documentID).
		DocumentRevisionId(-1).
		ClientToken(envelope.ClientToken).
		Body(larkdocx.NewCreateDocumentBlockChildrenReqBodyBuilder().
			Children(envelope.Children).
			Index(envelope.Index).
			Build()).
		Build()
	resp, callErr := t.client.Docx.DocumentBlockChildren.Create(ctx, req)
	if callErr == nil && resp != nil && resp.Success() {
		if !durable {
			return nil
		}
		return t.recordDocxAppendSuccess(ctx, operation, executionToken)
	}
	firstErr := docxAppendResponseError(resp, callErr)
	if !retryableDocxAppendFailure(resp, callErr) {
		if !durable {
			return firstErr
		}
		if recovery {
			httpStatus, responseCode := docxAppendResponseStatus(resp)
			feishuToolsLog.Warn(ctx, "preserving uncertain feishu document append after recovery rejection account=%s request=%s document_ref=%s http_status=%d code=%d error_type=%T envelope_ref=%s",
				operation.AccountID, shortToolRequestID(requestID), hashString(documentID), httpStatus, responseCode, callErr, shortHash(operation.EnvelopeHash))
			return t.recordDocxAppendOutcomeUnknown(ctx, operation, executionToken, "recovery_rejected")
		}
		return t.recordDocxAppendFailure(ctx, operation, executionToken, "remote_rejected", firstErr)
	}
	httpStatus, responseCode := docxAppendResponseStatus(resp)
	operationContextEnded := ctx.Err() != nil
	feishuToolsLog.Warn(ctx, "retrying frozen feishu document append after uncertain response account=%s request=%s document_ref=%s attempt=1 http_status=%d code=%d operation_context_ended=%t error_type=%T envelope_ref=%s",
		t.accountID, shortToolRequestID(requestID), hashString(documentID), httpStatus, responseCode, operationContextEnded, callErr, shortHash(operation.EnvelopeHash))
	retryCtx, cancelRetry := context.WithTimeout(feishuidempotency.RetryContext(ctx), docxAppendReconciliationTimeout)
	defer cancelRetry()
	resp, callErr = t.client.Docx.DocumentBlockChildren.Create(retryCtx, req)
	if callErr == nil && resp != nil && resp.Success() {
		if !durable {
			return nil
		}
		return t.recordDocxAppendSuccess(ctx, operation, executionToken)
	}
	retryHTTPStatus, retryResponseCode := docxAppendResponseStatus(resp)
	feishuToolsLog.Warn(ctx, "feishu document append outcome remains unknown after frozen retry account=%s request=%s document_ref=%s retry_http_status=%d retry_code=%d retry_error_type=%T envelope_ref=%s",
		t.accountID, shortToolRequestID(requestID), hashString(documentID), retryHTTPStatus, retryResponseCode, callErr, shortHash(operation.EnvelopeHash))
	if durable {
		return t.recordDocxAppendOutcomeUnknown(ctx, operation, executionToken, "uncertain_append_response")
	}
	return fmt.Errorf("%w after same-token frozen reconciliation request", errDocxAppendOutcomeUnknown)
}

func (t *docsService) recordDocxAppendSuccess(ctx context.Context, operation store.FeishuDocxAppendOperation, executionToken string) error {
	completed, err := t.store.MarkFeishuDocxAppendOperationSucceeded(operation.RequestID, operation.AccountID, executionToken, t.currentTime())
	if err != nil {
		feishuToolsLog.Warn(ctx, "persist feishu docx append success failed account=%s request=%s document_ref=%s error_type=%T",
			operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), err)
		return fmt.Errorf("%w after remote append success could not be persisted", errDocxAppendOutcomeUnknown)
	}
	feishuToolsLog.Info(ctx, "completed durable feishu docx append account=%s request=%s document_ref=%s state=%s payload_ref=%s",
		completed.AccountID, shortToolRequestID(completed.RequestID), hashString(completed.DocumentToken), completed.State, shortHash(completed.PayloadHash))
	return nil
}

func (t *docsService) recordDocxAppendFailure(ctx context.Context, operation store.FeishuDocxAppendOperation, executionToken, category string, remoteErr error) error {
	failed, err := t.store.MarkFeishuDocxAppendOperationFailed(operation.RequestID, operation.AccountID, executionToken, category, t.currentTime())
	if err != nil {
		if failed.State == store.FeishuDocxAppendOperationStateSucceeded {
			feishuToolsLog.Debug(ctx, "ignored stale feishu docx append rejection after authoritative success account=%s request=%s document_ref=%s",
				operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken))
			return nil
		}
		feishuToolsLog.Warn(ctx, "persist terminal feishu docx append failure failed account=%s request=%s document_ref=%s error_type=%T",
			operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), err)
		return fmt.Errorf("%w after terminal append result could not be persisted", errDocxAppendOutcomeUnknown)
	}
	feishuToolsLog.Warn(ctx, "failed durable feishu docx append account=%s request=%s document_ref=%s state=%s category=%s",
		failed.AccountID, shortToolRequestID(failed.RequestID), hashString(failed.DocumentToken), failed.State, failed.LastErrorCategory)
	return remoteErr
}

func (t *docsService) recordDocxAppendOutcomeUnknown(ctx context.Context, operation store.FeishuDocxAppendOperation, executionToken, category string) error {
	current, err := t.store.MarkFeishuDocxAppendOperationOutcomeUnknown(
		operation.RequestID,
		operation.AccountID,
		executionToken,
		category,
		t.currentTime(),
	)
	if err != nil {
		if current.State == store.FeishuDocxAppendOperationStateSucceeded {
			return nil
		}
		feishuToolsLog.Warn(ctx, "persist feishu docx append outcome_unknown failed account=%s request=%s document_ref=%s error_type=%T",
			operation.AccountID, shortToolRequestID(operation.RequestID), hashString(operation.DocumentToken), err)
	}
	return fmt.Errorf("%w after same-token frozen reconciliation request", errDocxAppendOutcomeUnknown)
}

func newDocxAppendExecutionToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate feishu docx append execution token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func docxAppendResponseStatus(resp *larkdocx.CreateDocumentBlockChildrenResp) (int, int) {
	if resp == nil {
		return 0, 0
	}
	status := 0
	if resp.ApiResp != nil {
		status = resp.ApiResp.StatusCode
	}
	return status, resp.Code
}

func shortHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
