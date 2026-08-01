package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFeishuBotResourcesAreAccountScopedAndUpdatable(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	resource, err := st.SaveFeishuBotResource(FeishuBotResource{
		AccountID:       "feishu:cli_test",
		ResourceType:    "folder",
		ResourceToken:   "fld_owned",
		Name:            "Owned",
		URL:             "https://docs.feishu.cn/drive/folder/fld_owned",
		SourceRequestID: "req_create",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("SaveFeishuBotResource returned error: %v", err)
	}
	if resource.ResourceToken != "fld_owned" || !resource.CreatedAt.Equal(now) {
		t.Fatalf("saved resource = %#v", resource)
	}
	if _, err := st.SaveFeishuBotResource(FeishuBotResource{
		AccountID:     resource.AccountID,
		ResourceType:  resource.ResourceType,
		ResourceToken: resource.ResourceToken,
		ParentToken:   "fld_parent",
		Name:          "Owned Updated",
		CreatedAt:     now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("update SaveFeishuBotResource returned error: %v", err)
	}
	got, err := st.GetFeishuBotResource(resource.AccountID, resource.ResourceType, resource.ResourceToken)
	if err != nil {
		t.Fatalf("GetFeishuBotResource returned error: %v", err)
	}
	if got.ParentToken != "fld_parent" || got.Name != "Owned Updated" || got.URL != resource.URL || got.SourceRequestID != resource.SourceRequestID {
		t.Fatalf("updated resource = %#v", got)
	}
	if _, err := st.GetFeishuBotResource("feishu:other", resource.ResourceType, resource.ResourceToken); !errors.Is(err, ErrFeishuBotResourceNotFound) {
		t.Fatalf("cross-account resource error = %v, want ErrFeishuBotResourceNotFound", err)
	}
}

func TestFeishuResourceAccessOAuthLifecycleAndChatScopedGrant(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	request, err := st.CreateFeishuResourceAccessRequest(FeishuResourceAccessRequest{
		AccountID:       "feishu:cli_test",
		ActorOpenID:     "ou_requester",
		ActorUserID:     "u_requester",
		ChatID:          "oc_chat",
		SourceMessageID: "om_source",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		ResourceURL:     "https://docs.feishu.cn/docx/doxcn_external",
		Permission:      FeishuResourcePermissionWrite,
		Reason:          "append the approved plan",
		CreatedAt:       now,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
	}
	if !strings.HasPrefix(request.ID, "req_") || request.State != FeishuResourceAccessStatePending {
		t.Fatalf("created request = %#v", request)
	}
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, "state_hash", "pkce_verifier", "openid", "ou_bot", now.Add(time.Second)); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "om_card", now.Add(2*time.Second)); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	claimed, err := st.ClaimFeishuResourceAccessOAuth("state_hash", request.AccountID, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ClaimFeishuResourceAccessOAuth returned error: %v", err)
	}
	if claimed.State != FeishuResourceAccessStateExecuting || claimed.PKCEVerifier != "pkce_verifier" || claimed.SubjectID != "ou_bot" {
		t.Fatalf("claimed request = %#v", claimed)
	}
	stored, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		t.Fatalf("GetFeishuResourceAccessRequest returned error: %v", err)
	}
	if stored.PKCEVerifier != "" || stored.OAuthStateHash != "" || stored.State != FeishuResourceAccessStateExecuting {
		t.Fatalf("claimed stored request retained one-time values: %#v", stored)
	}
	if _, err := st.ClaimFeishuResourceAccessOAuth("state_hash", request.AccountID, now.Add(4*time.Second)); !errors.Is(err, ErrFeishuResourceAccessNotFound) {
		t.Fatalf("second OAuth claim error = %v, want ErrFeishuResourceAccessNotFound", err)
	}

	grant := FeishuResourceGrant{
		AccountID:       request.AccountID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      FeishuResourcePermissionWrite,
		SubjectType:     "openid",
		SubjectID:       "ou_bot",
		SourceRequestID: request.ID,
		State:           FeishuResourceGrantStateActive,
		CreatedAt:       now.Add(4 * time.Second),
		VerifiedAt:      now.Add(4 * time.Second),
	}
	if err := st.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		FeishuResourceGrantSourceNewlyGranted,
		FeishuResourcePermissionWrite,
		&grant,
		now.Add(4*time.Second),
	); err != nil {
		t.Fatalf("CompleteFeishuResourceAccessRequest returned error: %v", err)
	}
	completed, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || completed.State != FeishuResourceAccessStateSucceeded || completed.GrantSource != FeishuResourceGrantSourceNewlyGranted {
		t.Fatalf("completed request = %#v err=%v", completed, err)
	}
	workflow, err := st.GetWorkflowRequest(request.ID, request.AccountID)
	if err != nil || workflow.Kind != WorkflowRequestKindFeishuResourceAccess || workflow.State != WorkflowRequestStateSucceeded {
		t.Fatalf("resource access workflow = %#v err=%v", workflow, err)
	}
	gotGrant, active, err := st.ActiveFeishuResourceGrant(request.AccountID, request.ChatID, request.ResourceType, request.ResourceToken, FeishuResourcePermissionRead)
	if err != nil || !active || gotGrant.Permission != FeishuResourcePermissionWrite {
		t.Fatalf("read through write grant = %#v active=%t err=%v", gotGrant, active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(request.AccountID, "oc_other", request.ResourceType, request.ResourceToken, FeishuResourcePermissionRead); err != nil || active {
		t.Fatalf("cross-chat grant active=%t err=%v, want false", active, err)
	}
}

func TestFeishuResourceAccessDenyValidatesActorAndCard(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	request := createPendingFeishuResourceAccess(t, st, now)
	match := FeishuResourceAccessMatch{
		ActorOpenID:   request.ActorOpenID,
		ActorUserID:   request.ActorUserID,
		ChatID:        request.ChatID,
		CardMessageID: request.CardMessageID,
	}
	wrongActor := match
	wrongActor.ActorOpenID = "ou_other"
	if _, err := st.DenyFeishuResourceAccessRequest(request.ID, request.AccountID, wrongActor, now.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessForbidden) {
		t.Fatalf("wrong actor error = %v, want ErrFeishuResourceAccessForbidden", err)
	}
	wrongCard := match
	wrongCard.CardMessageID = "om_other"
	if _, err := st.DenyFeishuResourceAccessRequest(request.ID, request.AccountID, wrongCard, now.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessContextMismatch) {
		t.Fatalf("wrong card error = %v, want ErrFeishuResourceAccessContextMismatch", err)
	}
	denied, err := st.DenyFeishuResourceAccessRequest(request.ID, request.AccountID, match, now.Add(time.Second))
	if err != nil || denied.State != FeishuResourceAccessStateDenied {
		t.Fatalf("denied request = %#v err=%v", denied, err)
	}
	if _, err := st.DenyFeishuResourceAccessRequest(request.ID, request.AccountID, match, now.Add(2*time.Second)); !errors.Is(err, ErrFeishuResourceAccessResolved) {
		t.Fatalf("second deny error = %v, want ErrFeishuResourceAccessResolved", err)
	}
}

func TestFeishuResourceGrantUpgradesButDoesNotDowngrade(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	base := FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      FeishuResourcePermissionWrite,
		SubjectType:     "openid",
		SubjectID:       "ou_bot",
		SourceRequestID: "req_write",
		State:           FeishuResourceGrantStateActive,
		CreatedAt:       now,
		VerifiedAt:      now,
	}
	if _, err := st.UpsertFeishuResourceGrant(base); err != nil {
		t.Fatalf("write UpsertFeishuResourceGrant returned error: %v", err)
	}
	base.Permission = FeishuResourcePermissionRead
	base.SourceRequestID = "req_read"
	base.VerifiedAt = now.Add(time.Minute)
	if _, err := st.UpsertFeishuResourceGrant(base); err != nil {
		t.Fatalf("read UpsertFeishuResourceGrant returned error: %v", err)
	}
	got, active, err := st.ActiveFeishuResourceGrant(base.AccountID, base.ChatID, base.ResourceType, base.ResourceToken, FeishuResourcePermissionWrite)
	if err != nil || !active || got.Permission != FeishuResourcePermissionWrite || got.SourceRequestID != "req_read" {
		t.Fatalf("non-downgraded grant = %#v active=%t err=%v", got, active, err)
	}
	if err := st.RevokeFeishuResourceGrant(base.AccountID, base.ChatID, base.ResourceType, base.ResourceToken, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RevokeFeishuResourceGrant returned error: %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(base.AccountID, base.ChatID, base.ResourceType, base.ResourceToken, FeishuResourcePermissionRead); err != nil || active {
		t.Fatalf("revoked grant active=%t err=%v, want false", active, err)
	}
}

func TestFeishuResourceAccessRecoveryClearsOneTimeSecrets(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	expiring := createPendingFeishuResourceAccess(t, st, now)
	count, err := st.ExpireFeishuResourceAccessRequests(expiring.AccountID, expiring.ExpiresAt)
	if err != nil || count != 1 {
		t.Fatalf("ExpireFeishuResourceAccessRequests count=%d err=%v", count, err)
	}
	expired, err := st.GetFeishuResourceAccessRequest(expiring.ID, expiring.AccountID)
	if err != nil || expired.State != FeishuResourceAccessStateExpired || expired.PKCEVerifier != "" || expired.OAuthStateHash != "" {
		t.Fatalf("expired request = %#v err=%v", expired, err)
	}

	executing := createPendingFeishuResourceAccess(t, st, now.Add(time.Hour))
	if _, err := st.ClaimFeishuResourceAccessOAuth("hash_"+executing.ID, executing.AccountID, executing.CreatedAt.Add(time.Second)); err != nil {
		t.Fatalf("ClaimFeishuResourceAccessOAuth returned error: %v", err)
	}
	count, err = st.FailExecutingFeishuResourceAccessRequests(executing.AccountID, executing.CreatedAt.Add(2*time.Second))
	if err != nil || count != 1 {
		t.Fatalf("FailExecutingFeishuResourceAccessRequests count=%d err=%v", count, err)
	}
	failed, err := st.GetFeishuResourceAccessRequest(executing.ID, executing.AccountID)
	if err != nil || failed.State != FeishuResourceAccessStateFailed {
		t.Fatalf("failed executing request = %#v err=%v", failed, err)
	}
}

func createPendingFeishuResourceAccess(t *testing.T, st *Store, now time.Time) FeishuResourceAccessRequest {
	t.Helper()
	request, err := st.CreateFeishuResourceAccessRequest(FeishuResourceAccessRequest{
		AccountID:     "feishu:cli_test",
		ActorOpenID:   "ou_requester",
		ActorUserID:   "u_requester",
		ChatID:        "oc_chat",
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		Permission:    FeishuResourcePermissionRead,
		CreatedAt:     now,
		ExpiresAt:     now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
	}
	stateHash := "hash_" + request.ID
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, stateHash, "verifier_"+request.ID, "openid", "ou_bot", now); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "card_"+request.ID, now); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	request.OAuthStateHash = stateHash
	request.PKCEVerifier = "verifier_" + request.ID
	request.SubjectType = "openid"
	request.SubjectID = "ou_bot"
	request.CardMessageID = "card_" + request.ID
	return request
}
