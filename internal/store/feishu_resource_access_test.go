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
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, "state_hash", "", "openid", "ou_bot", now.Add(time.Second)); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "om_card", now.Add(2*time.Second)); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	claimed, err := st.ClaimFeishuResourceAccessOAuth("state_hash", request.AccountID, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ClaimFeishuResourceAccessOAuth returned error: %v", err)
	}
	if claimed.State != FeishuResourceAccessStateExecuting || claimed.PKCEVerifier != "" || claimed.SubjectID != "ou_bot" {
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
		ActorType:       FeishuResourceGrantActorTypeOpenID,
		ActorID:         request.ActorOpenID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      FeishuResourcePermissionWrite,
		GrantMode:       FeishuResourceGrantModeOnce,
		SourceRequestID: request.ID,
		State:           FeishuResourceGrantStateActive,
		ExpiresAt:       request.ExpiresAt,
		CreatedAt:       now.Add(4 * time.Second),
		UpdatedAt:       now.Add(4 * time.Second),
	}
	capability := FeishuResourceCapability{
		AccountID:         request.AccountID,
		ResourceType:      request.ResourceType,
		ResourceToken:     request.ResourceToken,
		SubjectType:       "openid",
		SubjectID:         "ou_bot",
		Permission:        FeishuResourcePermissionWrite,
		SourceActorOpenID: request.ActorOpenID,
		SourceActorUserID: request.ActorUserID,
		SourceRequestID:   request.ID,
		State:             FeishuResourceCapabilityStateActive,
		CreatedAt:         now.Add(4 * time.Second),
		VerifiedAt:        now.Add(4 * time.Second),
	}
	wrongActorGrant := grant
	wrongActorGrant.ActorID = "ou_other"
	if err := st.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		FeishuResourceGrantSourceNewlyGranted,
		FeishuResourcePermissionWrite,
		&capability,
		&wrongActorGrant,
		now.Add(4*time.Second),
	); err == nil || !strings.Contains(err.Error(), "access request actor") {
		t.Fatalf("cross-actor completion error = %v", err)
	}
	if err := st.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		FeishuResourceGrantSourceNewlyGranted,
		FeishuResourcePermissionWrite,
		&capability,
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
	gotGrant, active, err := st.ActiveFeishuResourceGrant(
		request.AccountID, FeishuResourceGrantActorTypeOpenID, request.ActorOpenID,
		request.ChatID, request.ResourceType, request.ResourceToken, FeishuResourcePermissionRead, now.Add(5*time.Second),
	)
	if err != nil || !active || gotGrant.Permission != FeishuResourcePermissionWrite {
		t.Fatalf("read through write grant = %#v active=%t err=%v", gotGrant, active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		request.AccountID, FeishuResourceGrantActorTypeOpenID, request.ActorOpenID,
		"oc_other", request.ResourceType, request.ResourceToken, FeishuResourcePermissionRead, now.Add(5*time.Second),
	); err != nil || active {
		t.Fatalf("cross-chat grant active=%t err=%v, want false", active, err)
	}
}

func TestFeishuResourceAccessOAuthClaimReturnsAndClearsLegacyPKCEVerifier(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	request, err := st.CreateFeishuResourceAccessRequest(FeishuResourceAccessRequest{
		AccountID:     "feishu:cli_test",
		ActorOpenID:   "ou_requester",
		ChatID:        "oc_chat",
		ResourceType:  "docx",
		ResourceToken: "doxcn_external",
		Permission:    FeishuResourcePermissionWrite,
		CreatedAt:     now,
		ExpiresAt:     now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateFeishuResourceAccessRequest returned error: %v", err)
	}
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, "legacy_state_hash", "legacy_pkce_verifier", "openid", "ou_bot", now); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	claimed, err := st.ClaimFeishuResourceAccessOAuth("legacy_state_hash", request.AccountID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ClaimFeishuResourceAccessOAuth returned error: %v", err)
	}
	if claimed.PKCEVerifier != "legacy_pkce_verifier" {
		t.Fatalf("claimed legacy verifier = %q", claimed.PKCEVerifier)
	}
	stored, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		t.Fatalf("GetFeishuResourceAccessRequest returned error: %v", err)
	}
	if stored.PKCEVerifier != "" || stored.OAuthStateHash != "" || stored.State != FeishuResourceAccessStateExecuting {
		t.Fatalf("claimed legacy request retained one-time values: %#v", stored)
	}
}

func TestFeishuResourceAccessConsumptionIsAtomicAndExpires(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	request := createPendingFeishuResourceAccess(t, st, now)
	if err := st.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		FeishuResourceGrantSourceBotOwner,
		request.Permission,
		nil,
		nil,
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("CompleteFeishuResourceAccessRequest returned error: %v", err)
	}
	workflow, err := st.CreateWorkflowRequest(WorkflowRequest{
		AccountID: request.AccountID,
		Kind:      WorkflowRequestKindFeishuDocsCreate,
		State:     WorkflowRequestStateExecuting,
		CreatedAt: now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("CreateWorkflowRequest returned error: %v", err)
	}
	consumedAt := now.Add(3 * time.Second)
	if err := st.ConsumeFeishuResourceAccessRequest(request.ID, request.AccountID, workflow.ID, consumedAt); err != nil {
		t.Fatalf("ConsumeFeishuResourceAccessRequest returned error: %v", err)
	}
	consumed, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		t.Fatalf("GetFeishuResourceAccessRequest returned error: %v", err)
	}
	if consumed.ConsumedByRequestID != workflow.ID || !consumed.ConsumedAt.Equal(consumedAt) {
		t.Fatalf("consumed request = %#v", consumed)
	}
	if err := st.ConsumeFeishuResourceAccessRequest(request.ID, request.AccountID, "req_second", consumedAt.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessConsumed) {
		t.Fatalf("second consumption error = %v, want ErrFeishuResourceAccessConsumed", err)
	}

	expired := createPendingFeishuResourceAccess(t, st, now.Add(time.Hour))
	if err := st.CompleteFeishuResourceAccessRequest(
		expired.ID,
		expired.AccountID,
		FeishuResourceGrantSourceBotOwner,
		expired.Permission,
		nil,
		nil,
		expired.CreatedAt.Add(time.Second),
	); err != nil {
		t.Fatalf("complete expiring access request: %v", err)
	}
	if err := st.ConsumeFeishuResourceAccessRequest(expired.ID, expired.AccountID, "req_after_expiry", expired.ExpiresAt); !errors.Is(err, ErrFeishuResourceAccessExpired) {
		t.Fatalf("expired consumption error = %v, want ErrFeishuResourceAccessExpired", err)
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

func TestFeishuResourceAccessOAuthCardClaimValidatesContextStateAndExpiry(t *testing.T) {
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
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(request.ID, request.AccountID, request.OAuthStateHash, wrongActor, now.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessForbidden) {
		t.Fatalf("wrong actor claim error = %v, want ErrFeishuResourceAccessForbidden", err)
	}
	wrongCard := match
	wrongCard.CardMessageID = "om_other"
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(request.ID, request.AccountID, request.OAuthStateHash, wrongCard, now.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessContextMismatch) {
		t.Fatalf("wrong card claim error = %v, want ErrFeishuResourceAccessContextMismatch", err)
	}
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(request.ID, request.AccountID, "wrong_state_hash", match, now.Add(time.Second)); !errors.Is(err, ErrFeishuResourceAccessOAuthStateMismatch) {
		t.Fatalf("wrong state claim error = %v, want ErrFeishuResourceAccessOAuthStateMismatch", err)
	}
	pending, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || pending.State != FeishuResourceAccessStatePending || pending.OAuthStateHash == "" || pending.PKCEVerifier != "" {
		t.Fatalf("rejected card claims changed pending request = %#v err=%v", pending, err)
	}

	claimed, err := st.ClaimFeishuResourceAccessOAuthFromCard(request.ID, request.AccountID, request.OAuthStateHash, match, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("ClaimFeishuResourceAccessOAuthFromCard returned error: %v", err)
	}
	if claimed.State != FeishuResourceAccessStateExecuting || claimed.PKCEVerifier != "" || claimed.OAuthStateHash != "" {
		t.Fatalf("claimed card request = %#v", claimed)
	}
	stored, err := st.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil || stored.State != FeishuResourceAccessStateExecuting || stored.OAuthStateHash != "" || stored.PKCEVerifier != "" {
		t.Fatalf("stored card claim retained one-time values = %#v err=%v", stored, err)
	}
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(request.ID, request.AccountID, "", match, now.Add(3*time.Second)); !errors.Is(err, ErrFeishuResourceAccessResolved) {
		t.Fatalf("duplicate card claim error = %v, want ErrFeishuResourceAccessResolved", err)
	}

	rawCodeRequest := createPendingFeishuResourceAccess(t, st, now.Add(time.Hour))
	rawMatch := FeishuResourceAccessMatch{
		ActorOpenID:   rawCodeRequest.ActorOpenID,
		ActorUserID:   rawCodeRequest.ActorUserID,
		ChatID:        rawCodeRequest.ChatID,
		CardMessageID: rawCodeRequest.CardMessageID,
	}
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(rawCodeRequest.ID, rawCodeRequest.AccountID, "", rawMatch, rawCodeRequest.CreatedAt.Add(time.Second)); err != nil {
		t.Fatalf("raw-code card claim returned error: %v", err)
	}

	expiredRequest := createPendingFeishuResourceAccess(t, st, now.Add(2*time.Hour))
	expiredMatch := FeishuResourceAccessMatch{
		ActorOpenID:   expiredRequest.ActorOpenID,
		ActorUserID:   expiredRequest.ActorUserID,
		ChatID:        expiredRequest.ChatID,
		CardMessageID: expiredRequest.CardMessageID,
	}
	if _, err := st.ClaimFeishuResourceAccessOAuthFromCard(expiredRequest.ID, expiredRequest.AccountID, expiredRequest.OAuthStateHash, expiredMatch, expiredRequest.ExpiresAt); !errors.Is(err, ErrFeishuResourceAccessExpired) {
		t.Fatalf("expired card claim error = %v, want ErrFeishuResourceAccessExpired", err)
	}
	expired, err := st.GetFeishuResourceAccessRequest(expiredRequest.ID, expiredRequest.AccountID)
	if err != nil || expired.State != FeishuResourceAccessStateExpired || expired.OAuthStateHash != "" || expired.PKCEVerifier != "" {
		t.Fatalf("expired card claim = %#v err=%v", expired, err)
	}
}

func TestFeishuResourceGrantSeparatesPermissionAndOnceAllLifetimes(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	base := FeishuResourceGrant{
		AccountID:       "feishu:cli_test",
		ActorType:       FeishuResourceGrantActorTypeOpenID,
		ActorID:         "ou_requester",
		ChatID:          "oc_chat",
		ResourceType:    "docx",
		ResourceToken:   "doxcn_external",
		Permission:      FeishuResourcePermissionWrite,
		GrantMode:       FeishuResourceGrantModeOnce,
		SourceRequestID: "req_write",
		State:           FeishuResourceGrantStateActive,
		ExpiresAt:       now.Add(20 * time.Minute),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := st.UpsertFeishuResourceGrant(base); err != nil {
		t.Fatalf("write UpsertFeishuResourceGrant returned error: %v", err)
	}
	readAll := base
	readAll.Permission = FeishuResourcePermissionRead
	readAll.GrantMode = FeishuResourceGrantModeAll
	readAll.ExpiresAt = time.Time{}
	readAll.SourceRequestID = "req_read_all"
	readAll.UpdatedAt = now.Add(time.Minute)
	if _, err := st.UpsertFeishuResourceGrant(readAll); err != nil {
		t.Fatalf("read UpsertFeishuResourceGrant returned error: %v", err)
	}
	gotRead, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionRead, now.Add(2*time.Minute),
	)
	if err != nil || !active || gotRead.Permission != FeishuResourcePermissionRead || gotRead.GrantMode != FeishuResourceGrantModeAll {
		t.Fatalf("preferred permanent read grant = %#v active=%t err=%v", gotRead, active, err)
	}
	gotWrite, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionWrite, now.Add(2*time.Minute),
	)
	if err != nil || !active || gotWrite.Permission != FeishuResourcePermissionWrite || gotWrite.GrantMode != FeishuResourceGrantModeOnce {
		t.Fatalf("temporary write grant = %#v active=%t err=%v", gotWrite, active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, "ou_other", base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionRead, now.Add(2*time.Minute),
	); err != nil || active {
		t.Fatalf("cross-actor grant active=%t err=%v, want false", active, err)
	}
	if count, err := st.ExpireFeishuResourceGrants(base.AccountID, now.Add(21*time.Minute)); err != nil || count != 1 {
		t.Fatalf("ExpireFeishuResourceGrants count=%d err=%v", count, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionWrite, now.Add(21*time.Minute),
	); err != nil || active {
		t.Fatalf("expired write grant active=%t err=%v, want false", active, err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionRead, now.Add(21*time.Minute),
	); err != nil || !active {
		t.Fatalf("permanent read grant active=%t err=%v, want true", active, err)
	}
	writeAll := base
	writeAll.GrantMode = FeishuResourceGrantModeAll
	writeAll.ExpiresAt = time.Time{}
	writeAll.SourceRequestID = "req_write_all"
	writeAll.CreatedAt = now.Add(22 * time.Minute)
	writeAll.UpdatedAt = writeAll.CreatedAt
	if _, err := st.UpsertFeishuResourceGrant(writeAll); err != nil {
		t.Fatalf("permanent write UpsertFeishuResourceGrant returned error: %v", err)
	}
	writeOnceAgain := base
	writeOnceAgain.SourceRequestID = "req_write_once_again"
	writeOnceAgain.CreatedAt = now.Add(23 * time.Minute)
	writeOnceAgain.UpdatedAt = writeOnceAgain.CreatedAt
	writeOnceAgain.ExpiresAt = now.Add(40 * time.Minute)
	if _, err := st.UpsertFeishuResourceGrant(writeOnceAgain); err != nil {
		t.Fatalf("write once after all UpsertFeishuResourceGrant returned error: %v", err)
	}
	gotWrite, active, err = st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionWrite, now.Add(24*time.Minute),
	)
	if err != nil || !active || gotWrite.GrantMode != FeishuResourceGrantModeAll || !gotWrite.ExpiresAt.IsZero() {
		t.Fatalf("permanent write grant after once update = %#v active=%t err=%v", gotWrite, active, err)
	}
	if err := st.RevokeFeishuResourceGrant(base.AccountID, base.ChatID, base.ResourceType, base.ResourceToken, now.Add(25*time.Minute)); err != nil {
		t.Fatalf("RevokeFeishuResourceGrant returned error: %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceGrant(
		base.AccountID, base.ActorType, base.ActorID, base.ChatID,
		base.ResourceType, base.ResourceToken, FeishuResourcePermissionRead, now.Add(26*time.Minute),
	); err != nil || active {
		t.Fatalf("revoked grant active=%t err=%v, want false", active, err)
	}
}

func TestFeishuResourceCapabilityIsSubjectScopedAndDoesNotDowngrade(t *testing.T) {
	st := openFeishuDocsTestStore(t)
	now := time.Date(2026, time.August, 1, 13, 0, 0, 0, time.UTC)
	base := FeishuResourceCapability{
		AccountID:         "feishu:cli_test",
		ResourceType:      "folder",
		ResourceToken:     "fld_external",
		SubjectType:       "openid",
		SubjectID:         "ou_bot",
		Permission:        FeishuResourcePermissionWrite,
		SourceActorOpenID: "ou_requester",
		SourceRequestID:   "req_write",
		State:             FeishuResourceCapabilityStateActive,
		CreatedAt:         now,
		VerifiedAt:        now,
	}
	if _, err := st.UpsertFeishuResourceCapability(base); err != nil {
		t.Fatalf("write UpsertFeishuResourceCapability returned error: %v", err)
	}
	base.Permission = FeishuResourcePermissionRead
	base.SourceRequestID = "req_read"
	base.VerifiedAt = now.Add(time.Minute)
	if _, err := st.UpsertFeishuResourceCapability(base); err != nil {
		t.Fatalf("read UpsertFeishuResourceCapability returned error: %v", err)
	}
	got, active, err := st.ActiveFeishuResourceCapability(
		base.AccountID,
		base.ResourceType,
		base.ResourceToken,
		base.SubjectType,
		base.SubjectID,
		FeishuResourcePermissionWrite,
	)
	if err != nil || !active || got.Permission != FeishuResourcePermissionWrite || got.SourceRequestID != "req_read" {
		t.Fatalf("non-downgraded capability = %#v active=%t err=%v", got, active, err)
	}

	chatCapability := base
	chatCapability.SubjectType = "openchat"
	chatCapability.SubjectID = "oc_chat"
	chatCapability.Permission = FeishuResourcePermissionRead
	chatCapability.SourceRequestID = "req_chat"
	chatCapability.VerifiedAt = now.Add(2 * time.Minute)
	if _, err := st.UpsertFeishuResourceCapability(chatCapability); err != nil {
		t.Fatalf("chat UpsertFeishuResourceCapability returned error: %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		base.AccountID,
		base.ResourceType,
		base.ResourceToken,
		chatCapability.SubjectType,
		chatCapability.SubjectID,
		FeishuResourcePermissionWrite,
	); err != nil || active {
		t.Fatalf("chat write capability active=%t err=%v, want false", active, err)
	}
	if err := st.RevokeFeishuResourceCapability(
		base.AccountID,
		base.ResourceType,
		base.ResourceToken,
		base.SubjectType,
		base.SubjectID,
		now.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("RevokeFeishuResourceCapability returned error: %v", err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		base.AccountID,
		base.ResourceType,
		base.ResourceToken,
		base.SubjectType,
		base.SubjectID,
		FeishuResourcePermissionRead,
	); err != nil || active {
		t.Fatalf("revoked Bot capability active=%t err=%v, want false", active, err)
	}
	if _, active, err := st.ActiveFeishuResourceCapability(
		base.AccountID,
		base.ResourceType,
		base.ResourceToken,
		chatCapability.SubjectType,
		chatCapability.SubjectID,
		FeishuResourcePermissionRead,
	); err != nil || !active {
		t.Fatalf("independent chat capability active=%t err=%v, want true", active, err)
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
	if err := st.PrepareFeishuResourceAccessOAuth(request.ID, request.AccountID, stateHash, "", "openid", "ou_bot", now); err != nil {
		t.Fatalf("PrepareFeishuResourceAccessOAuth returned error: %v", err)
	}
	if err := st.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, "card_"+request.ID, now); err != nil {
		t.Fatalf("SetFeishuResourceAccessCardMessageID returned error: %v", err)
	}
	request.OAuthStateHash = stateHash
	request.SubjectType = "openid"
	request.SubjectID = "ou_bot"
	request.CardMessageID = "card_" + request.ID
	return request
}
