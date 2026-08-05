package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	"lingobridge/internal/store"
)

const (
	remoteCreateReconciliationPageSize = 200
	remoteCreateReconciliationMaxPages = 10
	remoteCreateClockSkew              = 2 * time.Minute
	remoteCreateWindowAfterStart       = 10 * time.Minute
	remoteCreateResultRecordPending    = "partial"
)

var defaultRemoteCreateReconciliationDelays = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

type remoteCreateCandidate struct {
	ResourceType string
	Token        string
	Name         string
	ParentToken  string
	URL          string
	OwnerID      string
	CreatedAt    time.Time
}

type remoteCreateStateAction uint8

const (
	remoteCreateStateActionInvalid remoteCreateStateAction = iota
	remoteCreateStateActionStart
	remoteCreateStateActionReconcile
	remoteCreateStateActionUseRecordedResult
	remoteCreateStateActionRejectFailed
)

// classifyRemoteCreateState is the shared document/folder create transition
// policy. Resource-specific code performs the action, but the durable ledger
// states must retain identical retry and reconciliation semantics.
func classifyRemoteCreateState(state string) remoteCreateStateAction {
	switch state {
	case store.FeishuRemoteOperationStatePrepared:
		return remoteCreateStateActionStart
	case store.FeishuRemoteOperationStateRemoteStarted,
		store.FeishuRemoteOperationStateReconcileRequired,
		store.FeishuRemoteOperationStateOutcomeUnknown:
		return remoteCreateStateActionReconcile
	case store.FeishuRemoteOperationStateRemoteSucceeded,
		store.FeishuRemoteOperationStatePersisted:
		return remoteCreateStateActionUseRecordedResult
	case store.FeishuRemoteOperationStateFailed:
		return remoteCreateStateActionRejectFailed
	default:
		return remoteCreateStateActionInvalid
	}
}

func remoteOperationPayloadHash(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal feishu remote operation payload hash: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func copyRemoteCreateReconciliationDelays() []time.Duration {
	return append([]time.Duration(nil), defaultRemoteCreateReconciliationDelays...)
}

func feishuCreateResponseUncertain(apiResp *larkcore.ApiResp, callErr error, responseMissingData bool) bool {
	if callErr != nil || apiResp == nil || responseMissingData {
		return true
	}
	status := apiResp.StatusCode
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func reconcileFeishuRemoteCreate(
	ctx context.Context,
	client *lark.Client,
	operation store.FeishuRemoteOperation,
	parent AuthorizedResource,
	expectedOwnerID string,
	delays []time.Duration,
) (remoteCreateCandidate, string, error) {
	if client == nil {
		return remoteCreateCandidate{}, "", fmt.Errorf("feishu remote create reconciliation client is required")
	}
	if err := validateRemoteOperationParentAccess(operation, parent); err != nil {
		return remoteCreateCandidate{}, "", err
	}
	if strings.TrimSpace(expectedOwnerID) == "" {
		root, err := getApplicationRootFolder(ctx, client)
		if err != nil {
			return remoteCreateCandidate{}, "", fmt.Errorf("resolve feishu application owner for reconciliation: %w", err)
		}
		expectedOwnerID = root.UserID
	}
	if strings.TrimSpace(expectedOwnerID) == "" {
		return remoteCreateCandidate{}, "", fmt.Errorf("feishu application owner is unavailable for conservative reconciliation")
	}
	attempts := len(delays) + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		candidates, err := listFeishuRemoteCreateCandidates(ctx, client, operation, parent, expectedOwnerID)
		if err == nil {
			switch len(candidates) {
			case 0:
				lastErr = nil
			case 1:
				return candidates[0], "unique_candidate", nil
			default:
				return remoteCreateCandidate{}, "multiple_candidates", nil
			}
		} else {
			lastErr = err
		}
		if attempt >= len(delays) {
			break
		}
		delay := delays[attempt]
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return remoteCreateCandidate{}, "", ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr != nil {
		return remoteCreateCandidate{}, "", lastErr
	}
	return remoteCreateCandidate{}, "no_candidate", nil
}

func listFeishuRemoteCreateCandidates(
	ctx context.Context,
	client *lark.Client,
	operation store.FeishuRemoteOperation,
	parent AuthorizedResource,
	expectedOwnerID string,
) ([]remoteCreateCandidate, error) {
	startedAt := operation.RemoteCallStartedAt
	if startedAt.IsZero() {
		startedAt = operation.CreatedAt
	}
	windowStart := startedAt.Add(-remoteCreateClockSkew)
	windowEnd := startedAt.Add(remoteCreateWindowAfterStart)
	pageToken := ""
	candidates := make([]remoteCreateCandidate, 0, 2)
	for page := 0; page < remoteCreateReconciliationMaxPages; page++ {
		builder := larkdrive.NewListFileReqBuilder().
			FolderToken(parent.ResourceToken).
			PageSize(remoteCreateReconciliationPageSize).
			OrderBy(larkdrive.OrderByCreatedTime).
			Direction(larkdrive.DirectionDESC)
		if pageToken != "" {
			builder.PageToken(pageToken)
		}
		resp, err := client.Drive.File.List(ctx, builder.Build())
		if err != nil {
			return nil, fmt.Errorf("list feishu parent for create reconciliation: %w", err)
		}
		if resp == nil {
			return nil, fmt.Errorf("list feishu parent for create reconciliation: empty response")
		}
		if !resp.Success() {
			return nil, fmt.Errorf("list feishu parent for create reconciliation code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data == nil || resp.Data.HasMore == nil {
			return nil, fmt.Errorf("list feishu parent for create reconciliation: incomplete pagination response")
		}
		for _, file := range resp.Data.Files {
			candidate, ok := matchingFeishuRemoteCreateCandidate(file, operation, expectedOwnerID, windowStart, windowEnd)
			if ok {
				candidates = append(candidates, candidate)
			}
		}
		if !*resp.Data.HasMore {
			return candidates, nil
		}
		pageToken = strings.TrimSpace(deref(resp.Data.NextPageToken))
		if pageToken == "" {
			return nil, fmt.Errorf("list feishu parent for create reconciliation: missing next_page_token")
		}
	}
	return nil, fmt.Errorf("list feishu parent for create reconciliation exceeded %d pages", remoteCreateReconciliationMaxPages)
}

func validateRemoteOperationParentAccess(operation store.FeishuRemoteOperation, parent AuthorizedResource) error {
	if strings.TrimSpace(operation.AccountID) == "" || strings.TrimSpace(operation.ChatID) == "" ||
		strings.TrimSpace(operation.ParentResourceType) == "" || strings.TrimSpace(operation.ParentResourceToken) == "" {
		return fmt.Errorf("feishu remote operation has incomplete trusted parent scope")
	}
	if parent.AccountID != operation.AccountID || parent.ChatID != operation.ChatID ||
		!authorizedResourcePermits(parent, operation.ParentResourceType, operation.ParentResourceToken, ResourcePermissionWrite) {
		return fmt.Errorf("authorized feishu parent resource does not match the remote operation")
	}
	if operation.ActorOpenID != "" && parent.ActorOpenID != operation.ActorOpenID {
		return fmt.Errorf("authorized feishu parent actor does not match the remote operation")
	}
	if operation.ActorUserID != "" && parent.ActorUserID != operation.ActorUserID {
		return fmt.Errorf("authorized feishu parent actor does not match the remote operation")
	}
	if operation.ActorOpenID == "" && operation.ActorUserID == "" {
		return fmt.Errorf("feishu remote operation has no trusted actor")
	}
	return nil
}

func matchingFeishuRemoteCreateCandidate(
	file *larkdrive.File,
	operation store.FeishuRemoteOperation,
	expectedOwnerID string,
	windowStart, windowEnd time.Time,
) (remoteCreateCandidate, bool) {
	if file == nil {
		return remoteCreateCandidate{}, false
	}
	resourceType := strings.TrimSpace(deref(file.Type))
	token := strings.TrimSpace(deref(file.Token))
	name := strings.TrimSpace(deref(file.Name))
	parentToken := strings.TrimSpace(deref(file.ParentToken))
	ownerID := strings.TrimSpace(deref(file.OwnerId))
	createdAt, err := parseFeishuRemoteCreatedTime(deref(file.CreatedTime))
	if err != nil || token == "" || resourceType != operation.RemoteResourceType || name != operation.RequestedName ||
		parentToken != operation.ParentResourceToken || ownerID != expectedOwnerID ||
		createdAt.Before(windowStart) || createdAt.After(windowEnd) {
		return remoteCreateCandidate{}, false
	}
	remoteURL := strings.TrimSpace(deref(file.Url))
	if remoteURL == "" {
		remoteURL = defaultRemoteCreateURL(resourceType, token)
	}
	return remoteCreateCandidate{
		ResourceType: resourceType,
		Token:        token,
		Name:         name,
		ParentToken:  parentToken,
		URL:          remoteURL,
		OwnerID:      ownerID,
		CreatedAt:    createdAt,
	}, true
}

func parseFeishuRemoteCreatedTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("missing created_time")
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if value > 1_000_000_000_000 {
			return time.UnixMilli(value).UTC(), nil
		}
		return time.Unix(value, 0).UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse created_time: %w", err)
	}
	return parsed.UTC(), nil
}

func getApplicationRootFolder(ctx context.Context, client *lark.Client) (applicationRootFolder, error) {
	if client == nil {
		return applicationRootFolder{}, fmt.Errorf("feishu client is required")
	}
	resp, err := client.Get(ctx, "/open-apis/drive/explorer/v2/root_folder/meta", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder: %w", err)
	}
	if resp == nil {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder: empty response")
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token  string `json:"token"`
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return applicationRootFolder{}, fmt.Errorf("parse feishu application root folder: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	root := applicationRootFolder{
		Token:  strings.TrimSpace(result.Data.Token),
		ID:     strings.TrimSpace(result.Data.ID),
		UserID: strings.TrimSpace(result.Data.UserID),
	}
	if root.Token == "" {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder returned no token")
	}
	return root, nil
}

func trustedRemoteOperationMatches(operation store.FeishuRemoteOperation, actor Actor, chat ChatContext) bool {
	if operation.ChatID != strings.TrimSpace(chat.ChatID) {
		return false
	}
	if operation.ActorOpenID != "" && operation.ActorOpenID != strings.TrimSpace(actor.OpenID) {
		return false
	}
	if operation.ActorUserID != "" && operation.ActorUserID != strings.TrimSpace(actor.UserID) {
		return false
	}
	return operation.ActorOpenID != "" || operation.ActorUserID != ""
}

func remoteCreateResourceClaimable(
	st *store.Store,
	operation store.FeishuRemoteOperation,
	resourceType, resourceToken string,
) (bool, error) {
	if st == nil {
		return false, fmt.Errorf("feishu remote operation store is required")
	}
	resourceType = strings.TrimSpace(resourceType)
	resourceToken = strings.TrimSpace(resourceToken)
	if resourceType == "" || resourceToken == "" {
		return false, fmt.Errorf("feishu remote operation resource type and token are required")
	}
	resource, err := st.GetFeishuBotResource(operation.AccountID, resourceType, resourceToken)
	if err == nil {
		if strings.TrimSpace(resource.SourceRequestID) != operation.RequestID {
			return false, nil
		}
	} else if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		return false, fmt.Errorf("check feishu remote resource ownership: %w", err)
	}
	switch resourceType {
	case "docx":
		document, err := st.GetFeishuChatDocument(operation.AccountID, operation.ChatID, resourceToken)
		if err == nil {
			return strings.TrimSpace(document.SourceRequestID) == operation.RequestID, nil
		}
		if !errors.Is(err, store.ErrFeishuChatDocumentNotFound) {
			return false, fmt.Errorf("check feishu remote document binding: %w", err)
		}
	case "folder":
		folder, err := st.GetFeishuChatFolder(operation.AccountID, operation.ChatID, resourceToken)
		if err == nil {
			return strings.TrimSpace(folder.CreateRequestID) == operation.RequestID, nil
		}
		if !errors.Is(err, store.ErrFeishuChatFolderNotFound) {
			return false, fmt.Errorf("check feishu remote folder binding: %w", err)
		}
	default:
		return false, fmt.Errorf("unsupported feishu remote resource type %q", resourceType)
	}
	return true, nil
}

func reconciledRemoteCreateCandidateClaimable(
	st *store.Store,
	operation store.FeishuRemoteOperation,
	candidate remoteCreateCandidate,
) (bool, string, error) {
	claimable, err := remoteCreateResourceClaimable(st, operation, candidate.ResourceType, candidate.Token)
	if err != nil || !claimable {
		return claimable, "candidate_claimed_by_another_request", err
	}
	if candidate.CreatedAt.IsZero() {
		return false, "candidate_missing_created_time", nil
	}
	competing, err := st.HasCompetingFeishuRemoteCreate(
		operation.RequestID,
		operation.AccountID,
		operation.ParentResourceType,
		operation.ParentResourceToken,
		operation.RequestedName,
		candidate.ResourceType,
		candidate.CreatedAt.Add(-remoteCreateWindowAfterStart),
		candidate.CreatedAt.Add(remoteCreateClockSkew),
	)
	if err != nil {
		return false, "", fmt.Errorf("check competing feishu remote create: %w", err)
	}
	if competing {
		return false, "candidate_matches_competing_request", nil
	}
	return true, "", nil
}

func recordReconciledFeishuRemoteCreate(
	ctx context.Context,
	st *store.Store,
	operation store.FeishuRemoteOperation,
	candidate remoteCreateCandidate,
	now time.Time,
) (store.FeishuRemoteOperation, string, error) {
	known := remoteOperationWithKnownResult(operation, candidate.ResourceType, candidate.Token, candidate.URL, now)
	recorded, err := st.RecordFeishuRemoteOperationSuccess(
		operation.RequestID,
		operation.AccountID,
		candidate.ResourceType,
		candidate.Token,
		candidate.URL,
		now,
	)
	if err == nil {
		return recorded, "", nil
	}
	if !errors.Is(err, store.ErrFeishuRemoteOperationConflict) {
		feishuToolsLog.Error(ctx, "record reconciled feishu remote create result failed account=%s request=%s type=%s resource_ref=%s: %v",
			operation.AccountID, shortToolRequestID(operation.RequestID), candidate.ResourceType, hashString(candidate.Token), err)
		return known, remoteCreateResultRecordPending, nil
	}
	unknown, markErr := st.MarkFeishuRemoteOperationOutcomeUnknown(
		operation.RequestID,
		operation.AccountID,
		"candidate_claimed_by_another_request",
		now,
	)
	if markErr != nil {
		return operation, "", fmt.Errorf("record conflicting feishu remote create candidate: %w", markErr)
	}
	return unknown, "outcome_unknown", nil
}

func recordDefiniteFeishuRemoteCreate(
	ctx context.Context,
	st *store.Store,
	operation store.FeishuRemoteOperation,
	resourceType, resourceToken, remoteURL string,
	now time.Time,
) (store.FeishuRemoteOperation, string) {
	known := remoteOperationWithKnownResult(operation, resourceType, resourceToken, remoteURL, now)
	recorded, err := st.RecordFeishuRemoteOperationSuccess(
		operation.RequestID,
		operation.AccountID,
		resourceType,
		resourceToken,
		remoteURL,
		now,
	)
	if err == nil {
		return recorded, ""
	}
	feishuToolsLog.Error(ctx, "record definite feishu remote create result failed account=%s request=%s type=%s resource_ref=%s: %v",
		operation.AccountID, shortToolRequestID(operation.RequestID), resourceType, hashString(resourceToken), err)
	return known, remoteCreateResultRecordPending
}

func remoteOperationWithKnownResult(
	operation store.FeishuRemoteOperation,
	resourceType, resourceToken, remoteURL string,
	now time.Time,
) store.FeishuRemoteOperation {
	operation.RemoteResourceType = strings.TrimSpace(resourceType)
	operation.RemoteResourceToken = strings.TrimSpace(resourceToken)
	operation.RemoteURL = strings.TrimSpace(remoteURL)
	operation.RemoteResultAt = now.UTC()
	return operation
}

func defaultRemoteCreateURL(resourceType, token string) string {
	switch strings.TrimSpace(resourceType) {
	case "folder":
		return "https://docs.feishu.cn/drive/folder/" + strings.TrimSpace(token)
	case "docx":
		return "https://docs.feishu.cn/docx/" + strings.TrimSpace(token)
	default:
		return ""
	}
}
