package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken/authorizationcode"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	defaultResourceAccessTTL        = 10 * time.Minute
	resourceAccessCallbackTimeout   = 30 * time.Second
	resourceAccessCardUpdateTimeout = 10 * time.Second

	resourceAccessOAuthScope = "auth:user.id:read docs:permission.member:create docs:permission.member:update offline_access"
)

type resourceAccessStore interface {
	PlatformID() string
	SaveFeishuBotResource(store.FeishuBotResource) (store.FeishuBotResource, error)
	GetFeishuBotResource(accountID, resourceType, resourceToken string) (store.FeishuBotResource, error)
	DefaultFeishuChatFolder(accountID, chatID string) (store.FeishuChatFolder, error)
	CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest) (store.FeishuResourceAccessRequest, error)
	CreateWorkflowContinuation(store.WorkflowContinuation) (store.WorkflowContinuation, error)
	CancelWorkflowContinuation(requestID, accountID, reason string, now time.Time) error
	StoreWorkflowResult(store.WorkflowResult) (store.WorkflowResult, store.WorkflowContinuation, bool, error)
	ListTerminalWorkflowResultGaps(accountID, kind string, updatedBefore time.Time, limit int) ([]store.WorkflowRequest, error)
	PrepareFeishuResourceAccessOAuth(id, accountID, stateHash, verifier, subjectType, subjectID string, now time.Time) error
	SetFeishuResourceAccessCardMessageID(id, accountID, messageID string, now time.Time) error
	ApproveFeishuResourceAccessRequest(id, accountID, grantMode string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	ListApprovedPendingFeishuResourceAccessRequests(accountID string, now time.Time, limit int) ([]store.FeishuResourceAccessRequest, error)
	GetFeishuResourceAccessRequest(id, accountID string) (store.FeishuResourceAccessRequest, error)
	ClaimFeishuResourceAccessOAuth(stateHash, accountID string, now time.Time) (store.FeishuResourceAccessRequest, error)
	ClaimFeishuResourceAccessOAuthFromCard(id, accountID, stateHash string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	DenyFeishuResourceAccessRequest(id, accountID string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	CompleteFeishuResourceAccessRequest(id, accountID, source, verifiedPermission string, capability *store.FeishuResourceCapability, grant *store.FeishuResourceGrant, now time.Time) error
	FailFeishuResourceAccessRequest(id, accountID string, now time.Time) error
	ExpireFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error)
	FailExecutingFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error)
	ExpireFeishuResourceGrants(accountID string, now time.Time) (int64, error)
	UpsertFeishuResourceGrant(store.FeishuResourceGrant) (store.FeishuResourceGrant, error)
	ActiveFeishuResourceGrant(accountID, actorType, actorID, chatID, resourceType, resourceToken, permission string, now time.Time) (store.FeishuResourceGrant, bool, error)
	RevokeFeishuResourceGrant(accountID, chatID, resourceType, resourceToken string, now time.Time) error
	UpsertFeishuResourceCapability(store.FeishuResourceCapability) (store.FeishuResourceCapability, error)
	ActiveFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID, permission string) (store.FeishuResourceCapability, bool, error)
	RevokeFeishuResourceCapability(accountID, resourceType, resourceToken, subjectType, subjectID string, now time.Time) error
	SaveFeishuUserOAuthCredential(store.FeishuUserOAuthCredential) (store.FeishuUserOAuthCredential, error)
	GetFeishuUserOAuthCredential(accountID, actorOpenID, actorUserID string) (store.FeishuUserOAuthCredential, error)
	RotateFeishuUserOAuthCredential(store.FeishuUserOAuthCredential, int64) (store.FeishuUserOAuthCredential, error)
	MarkFeishuUserOAuthCredentialReauthRequired(id, accountID string, expectedVersion int64, now time.Time) (store.FeishuUserOAuthCredential, error)
}

type resourceAccessOAuthConfig struct {
	ClientID              string
	BaseURL               string
	CallbackURL           string
	CallbackListenAddress string
	CredentialSecret      string
}

type resourceAccessManager struct {
	store     resourceAccessStore
	client    *lark.Client
	cards     CardService
	account   store.Account
	botOpenID string
	oauth     resourceAccessOAuthConfig
	runCtx    context.Context
	ttl       time.Duration
	now       func() time.Time

	credentialCipher *feishuOAuthCredentialCipher
	credentialMu     sync.Mutex
}

var _ feishutools.ResourceAccessController = (*resourceAccessManager)(nil)

func newResourceAccessManager(
	runCtx context.Context,
	st *store.Store,
	client *lark.Client,
	account store.Account,
	botOpenID string,
	cards CardService,
	oauth resourceAccessOAuthConfig,
) (*resourceAccessManager, error) {
	if st == nil || st.PlatformID() != store.PlatformFeishu {
		return nil, fmt.Errorf("feishu resource access requires a Feishu store")
	}
	if client == nil {
		return nil, fmt.Errorf("feishu resource access client is required")
	}
	if strings.TrimSpace(account.ID) == "" || strings.TrimSpace(botOpenID) == "" {
		return nil, fmt.Errorf("feishu resource access account and bot open_id are required")
	}
	if cards == nil {
		return nil, fmt.Errorf("feishu resource access cards are required")
	}
	oauth.ClientID = strings.TrimSpace(oauth.ClientID)
	oauth.BaseURL = strings.TrimRight(strings.TrimSpace(oauth.BaseURL), "/")
	oauth.CallbackURL = strings.TrimSpace(oauth.CallbackURL)
	oauth.CallbackListenAddress = strings.TrimSpace(oauth.CallbackListenAddress)
	if oauth.CallbackURL == "" && oauth.CallbackListenAddress != "" {
		return nil, fmt.Errorf("feishu oauth_callback_listen_address requires oauth_callback_url")
	}
	if oauth.CallbackURL != "" {
		parsed, err := url.Parse(oauth.CallbackURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("feishu oauth_callback_url must be an absolute URL")
		}
		if oauth.ClientID == "" || oauth.BaseURL == "" {
			return nil, fmt.Errorf("feishu OAuth client_id and oauth_base_url are required when the callback is enabled")
		}
		if strings.TrimSpace(oauth.CredentialSecret) == "" {
			return nil, fmt.Errorf("feishu app secret is required to encrypt persisted OAuth credentials")
		}
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	var credentialCipher *feishuOAuthCredentialCipher
	if oauth.CallbackURL != "" {
		cipherValue, err := newFeishuOAuthCredentialCipher(oauth.CredentialSecret, account.ID)
		if err != nil {
			return nil, fmt.Errorf("initialize feishu OAuth credential encryption: %w", err)
		}
		credentialCipher = cipherValue
	}
	manager := &resourceAccessManager{
		store:            st,
		client:           client,
		cards:            cards,
		account:          account,
		botOpenID:        strings.TrimSpace(botOpenID),
		oauth:            oauth,
		runCtx:           runCtx,
		ttl:              defaultResourceAccessTTL,
		now:              time.Now,
		credentialCipher: credentialCipher,
	}
	if err := cards.RegisterAction(resourceAccessCardActionKind, manager.HandleCardAction); err != nil {
		return nil, fmt.Errorf("register feishu resource access card action: %w", err)
	}
	return manager, nil
}

func (m *resourceAccessManager) recoverPersistedRequests(ctx context.Context) error {
	now := m.currentTime()
	expiredGrants, err := m.store.ExpireFeishuResourceGrants(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu resource grants: %w", err)
	}
	expired, err := m.store.ExpireFeishuResourceAccessRequests(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("expire persisted feishu resource access requests: %w", err)
	}
	interrupted, err := m.store.FailExecutingFeishuResourceAccessRequests(m.account.ID, now)
	if err != nil {
		return fmt.Errorf("close interrupted feishu resource access requests: %w", err)
	}
	if expired > 0 {
		feishuLog.Info(ctx, "expired persisted feishu resource access requests account=%s count=%d", m.account.ID, expired)
	}
	if expiredGrants > 0 {
		feishuLog.Info(ctx, "expired persisted feishu resource grants account=%s count=%d", m.account.ID, expiredGrants)
	}
	if interrupted > 0 {
		feishuLog.Warn(ctx, "closed interrupted feishu resource access requests account=%s count=%d", m.account.ID, interrupted)
	}
	approved, err := m.store.ListApprovedPendingFeishuResourceAccessRequests(m.account.ID, now, 100)
	if err != nil {
		return fmt.Errorf("list approved pending feishu resource access requests: %w", err)
	}
	for _, request := range approved {
		if err := m.completeApprovedResourceAccess(ctx, request); err != nil {
			m.finishResourceAccessFailure(
				ctx,
				request,
				err,
				"资源授权恢复失败",
				"LingoBridge 重启后未能继续本次资源授权，请重新调用资源授权工具。",
			)
		}
	}
	if len(approved) > 0 {
		feishuLog.Info(ctx, "resumed approved feishu resource access requests account=%s count=%d", m.account.ID, len(approved))
	}
	reconciled, err := m.reconcileTerminalResourceAccessResults(ctx, now)
	if err != nil {
		return fmt.Errorf("reconcile persisted feishu resource access results: %w", err)
	}
	if reconciled > 0 {
		feishuLog.Info(ctx, "reconciled persisted feishu resource access results account=%s count=%d", m.account.ID, reconciled)
	}
	return nil
}

func (m *resourceAccessManager) RequestAccess(ctx context.Context, input feishutools.ResourceAccessRequest) (feishutools.ResourceAccessResult, error) {
	actor, chat, err := trustedResourceAccessScope(ctx)
	if err != nil {
		return feishutools.ResourceAccessResult{}, err
	}
	input, err = normalizeResourceAccessRequest(input)
	if err != nil {
		return feishutools.ResourceAccessResult{}, err
	}
	resourceType, resourceToken, resourceURL, err := m.resolveResource(ctx, chat, input)
	if err != nil {
		return feishutools.ResourceAccessResult{}, err
	}
	subjectType, subjectID, supportedMessage := m.resourceGrantSubject(chat, resourceType)
	now := m.currentTime()
	request, err := m.store.CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest{
		AccountID:           m.account.ID,
		ActorOpenID:         actor.OpenID,
		ActorUserID:         actor.UserID,
		ChatID:              chat.ChatID,
		SourceMessageID:     chat.MessageID,
		ResourceType:        resourceType,
		ResourceToken:       resourceToken,
		ResourceURL:         resourceURL,
		Permission:          input.Permission,
		Reason:              input.Reason,
		OnceDurationMinutes: input.OnceDurationMinutes,
		SubjectType:         subjectType,
		SubjectID:           subjectID,
		CreatedAt:           now,
		ExpiresAt:           now.Add(m.requestTTL()),
	})
	if err != nil {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("persist feishu resource access request: %w", err)
	}
	result := m.resourceAccessResult(request, feishutools.ResourceAccessStatusPending, "")

	if _, err := m.store.GetFeishuBotResource(m.account.ID, resourceType, resourceToken); err == nil {
		if err := m.store.CompleteFeishuResourceAccessRequest(
			request.ID,
			request.AccountID,
			store.FeishuResourceGrantSourceBotOwner,
			request.Permission,
			nil,
			nil,
			m.currentTime(),
		); err != nil {
			return feishutools.ResourceAccessResult{}, fmt.Errorf("complete Bot-owned resource access: %w", err)
		}
		feishuLog.Debug(ctx, "granted Bot-owned feishu resource request=%s account=%s chat=%s type=%s resource_ref=%s permission=%s",
			shortRequestID(request.ID), request.AccountID, request.ChatID, resourceType, shortResourceRef(resourceToken), request.Permission)
		return m.resourceAccessResult(request, feishutools.ResourceAccessStatusGranted, store.FeishuResourceGrantSourceBotOwner), nil
	} else if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("check Bot-owned feishu resource: %w", err)
	}

	actorType, actorID, err := resourceAccessGrantActor(actor.OpenID, actor.UserID)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, err
	}
	grant, active, err := m.store.ActiveFeishuResourceGrant(
		request.AccountID,
		actorType,
		actorID,
		request.ChatID,
		resourceType,
		resourceToken,
		request.Permission,
		now,
	)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("check feishu resource grant: %w", err)
	}
	if active && subjectType != "" {
		capability, capable, capabilityErr := m.store.ActiveFeishuResourceCapability(
			request.AccountID,
			request.ResourceType,
			request.ResourceToken,
			subjectType,
			subjectID,
			request.Permission,
		)
		if capabilityErr != nil {
			m.failResourceAccessBestEffort(ctx, request.ID)
			return feishutools.ResourceAccessResult{}, fmt.Errorf("check feishu resource capability: %w", capabilityErr)
		}
		verified := false
		var verifyErr error
		if capable {
			verified, verifyErr = m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, request.Permission, capability.SubjectType, capability.SubjectID)
		}
		if verifyErr != nil {
			feishuLog.Warn(ctx, "live-check existing feishu resource capability failed request=%s account=%s chat=%s type=%s resource_ref=%s: %v",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType, shortResourceRef(request.ResourceToken), verifyErr)
			m.failResourceAccessBestEffort(ctx, request.ID)
			return feishutools.ResourceAccessResult{}, fmt.Errorf("live-check existing feishu resource capability: %w", verifyErr)
		}
		if verified {
			capability.SourceRequestID = request.ID
			capability.VerifiedAt = m.currentTime()
			capability.UpdatedAt = capability.VerifiedAt
			grant.SourceRequestID = request.ID
			grant.UpdatedAt = capability.VerifiedAt
			if err := m.store.CompleteFeishuResourceAccessRequest(
				request.ID,
				request.AccountID,
				store.FeishuResourceGrantSourceExistingGrant,
				grant.Permission,
				&capability,
				&grant,
				capability.VerifiedAt,
			); err != nil {
				return feishutools.ResourceAccessResult{}, fmt.Errorf("complete existing feishu resource grant: %w", err)
			}
			feishuLog.Info(ctx, "reused verified feishu resource capability and grant request=%s account=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType, shortResourceRef(request.ResourceToken), grant.Permission, capability.SubjectType)
			return m.resourceAccessResult(request, feishutools.ResourceAccessStatusGranted, store.FeishuResourceGrantSourceExistingGrant), nil
		}
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, subjectType, subjectID, "stale existing access")
	} else if active {
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, subjectType, subjectID, "unsupported existing access subject")
	}

	if subjectType == "" {
		m.failResourceAccessBestEffort(ctx, request.ID)
		result.Status = feishutools.ResourceAccessStatusUnsupported
		result.Message = supportedMessage
		return result, nil
	}
	_, capabilityActive, err := m.store.ActiveFeishuResourceCapability(
		request.AccountID,
		request.ResourceType,
		request.ResourceToken,
		subjectType,
		subjectID,
		request.Permission,
	)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("check feishu resource capability before authorization card: %w", err)
	}
	if !capabilityActive && !m.oauthEnabled() {
		m.failResourceAccessBestEffort(ctx, request.ID)
		result.Status = feishutools.ResourceAccessStatusUnsupported
		result.Message = "当前机器人账号未配置 OAuth，且没有可复用的飞书资源能力；只能直接使用 Bot 自有资源或已经可以实时验证的授权。"
		return result, nil
	}
	execution, err := trustedWorkflowExecutionContext(ctx, m.account.ID, feishutools.ResourceAccessToolName)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, err
	}
	if _, err := persistWorkflowContinuation(m.store, execution, request.ID, m.currentTime()); err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, err
	}
	messageID, err := m.cards.Send(ctx, request.ChatID, pendingResourceGrantCard{request: request})
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, request.ID, request.AccountID, "resource access card send failed", m.currentTime())
		return feishutools.ResourceAccessResult{}, fmt.Errorf("send feishu resource access card: %w", err)
	}
	if err := m.store.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, messageID, m.currentTime()); err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		cancelWorkflowContinuationBestEffort(ctx, m.store, request.ID, request.AccountID, "resource access card binding failed", m.currentTime())
		m.updateResourceCardBestEffort(ctx, messageID, statusCard{title: "授权请求失败", template: "red", message: "授权请求未能保存，请重新发起。"})
		return feishutools.ResourceAccessResult{}, fmt.Errorf("bind feishu resource access card: %w", err)
	}
	request.CardMessageID = messageID
	feishuLog.Info(ctx, "requested feishu resource authorization choice request=%s account=%s user=%s chat=%s card_message=%s type=%s resource_ref=%s permission=%s once_minutes=%d capability_present=%t oauth_enabled=%t subject_type=%s expires_at=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		messageID, request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission,
		request.OnceDurationMinutes, capabilityActive, m.oauthEnabled(), request.SubjectType, request.ExpiresAt.Format(time.RFC3339))
	result.ExpiresAt = request.ExpiresAt
	result.Message = fmt.Sprintf("已向当前飞书用户发送资源授权卡片，请选择允许 %d 分钟、永久允许或拒绝。批准后 LingoBridge 会复用现有飞书能力或 OAuth 凭证；如需重新 OAuth，将在同一张卡片中继续。", request.OnceDurationMinutes)
	return result, nil
}

func (m *resourceAccessManager) RequireResourceAccess(ctx context.Context, requirement feishutools.ResourceAccessRequirement) error {
	actor, chat, err := trustedResourceAccessScope(ctx)
	if err != nil {
		return err
	}
	requirement.ResourceType = feishutools.NormalizeResourceType(requirement.ResourceType)
	requirement.ResourceToken = strings.TrimSpace(requirement.ResourceToken)
	requirement.Permission = strings.ToLower(strings.TrimSpace(requirement.Permission))
	if !feishutools.SupportedResourceType(requirement.ResourceType) || requirement.ResourceToken == "" ||
		(requirement.Permission != feishutools.ResourcePermissionRead && requirement.Permission != feishutools.ResourcePermissionWrite) {
		return fmt.Errorf("valid feishu resource type, token, and read/write permission are required")
	}
	if _, err := m.store.GetFeishuBotResource(m.account.ID, requirement.ResourceType, requirement.ResourceToken); err == nil {
		feishuLog.Debug(ctx, "allowed Bot-owned feishu resource access account=%s chat=%s type=%s resource_ref=%s permission=%s",
			m.account.ID, chat.ChatID, requirement.ResourceType, shortResourceRef(requirement.ResourceToken), requirement.Permission)
		return nil
	} else if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		return fmt.Errorf("check Bot-owned feishu resource: %w", err)
	}
	subjectType, subjectID, unsupportedMessage := m.resourceGrantSubject(chat, requirement.ResourceType)
	if subjectType == "" || subjectID == "" {
		return feishutools.NewResourceAuthorizationRequiredError(requirement, unsupportedMessage)
	}
	grantActorType, grantActorID, actorErr := resourceAccessGrantActor(actor.OpenID, actor.UserID)
	if actorErr != nil {
		return actorErr
	}
	_, activeGrant, grantErr := m.store.ActiveFeishuResourceGrant(
		m.account.ID,
		grantActorType,
		grantActorID,
		chat.ChatID,
		requirement.ResourceType,
		requirement.ResourceToken,
		requirement.Permission,
		m.currentTime(),
	)
	if grantErr != nil {
		return fmt.Errorf("load feishu resource grant: %w", grantErr)
	}
	if !activeGrant {
		return feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	capability, activeCapability, capabilityErr := m.store.ActiveFeishuResourceCapability(
		m.account.ID,
		requirement.ResourceType,
		requirement.ResourceToken,
		subjectType,
		subjectID,
		requirement.Permission,
	)
	if capabilityErr != nil {
		return fmt.Errorf("load feishu resource capability: %w", capabilityErr)
	}
	request := store.FeishuResourceAccessRequest{
		AccountID:     m.account.ID,
		ActorOpenID:   actor.OpenID,
		ActorUserID:   actor.UserID,
		ChatID:        chat.ChatID,
		ResourceType:  requirement.ResourceType,
		ResourceToken: requirement.ResourceToken,
		Permission:    requirement.Permission,
		SubjectType:   subjectType,
		SubjectID:     subjectID,
	}
	if !activeCapability {
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, subjectType, subjectID, "missing protected-tool capability")
		return feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	verified, verifyErr := m.verifyTenantAccess(ctx, requirement.ResourceType, requirement.ResourceToken, requirement.Permission, capability.SubjectType, capability.SubjectID)
	if verifyErr != nil {
		return fmt.Errorf("live-check feishu resource access: %w", verifyErr)
	}
	if !verified {
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, capability.SubjectType, capability.SubjectID, "invalid protected-tool access")
		return feishutools.NewResourceAuthorizationRequiredError(requirement, "")
	}
	verifiedAt := m.currentTime()
	capability.VerifiedAt = verifiedAt
	capability.UpdatedAt = verifiedAt
	if _, err := m.store.UpsertFeishuResourceCapability(capability); err != nil {
		return fmt.Errorf("refresh feishu resource capability verification: %w", err)
	}
	feishuLog.Debug(ctx, "allowed scoped feishu resource access account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s",
		m.account.ID, grantActorID, chat.ChatID, requirement.ResourceType,
		shortResourceRef(requirement.ResourceToken), requirement.Permission, capability.SubjectType)
	return nil
}

func (m *resourceAccessManager) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	requestID, action, ok := parseResourceAccessCardAction(event)
	if !ok {
		return nil, nil
	}
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Context == nil {
		return cardToast("error", "授权回调信息不完整，请重新发起。"), nil
	}
	operator := event.Event.Operator
	match := store.FeishuResourceAccessMatch{
		ActorOpenID:   operator.OpenID,
		ActorUserID:   deref(operator.UserID),
		ChatID:        event.Event.Context.OpenChatID,
		CardMessageID: event.Event.Context.OpenMessageID,
	}
	switch action {
	case resourceAccessCardActionApproveOnce, resourceAccessCardActionApproveAll:
		grantMode := store.FeishuResourceGrantModeOnce
		if action == resourceAccessCardActionApproveAll {
			grantMode = store.FeishuResourceGrantModeAll
		}
		request, err := m.store.ApproveFeishuResourceAccessRequest(requestID, m.account.ID, grantMode, match, m.currentTime())
		if err != nil {
			return m.resourceAccessDecisionError(ctx, request, requestID, err), nil
		}
		feishuLog.Info(ctx, "approved feishu resource authorization request=%s account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s grant_mode=%s once_minutes=%d",
			shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
			request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission, request.GrantMode, request.OnceDurationMinutes)
		go m.completeApprovedResourceAccessFromCard(request)
		return cardToast("success", "已记录授权选择，正在核验飞书权限；完成后会更新本卡片。"), nil
	case resourceAccessCardActionReject:
		request, err := m.store.DenyFeishuResourceAccessRequest(requestID, m.account.ID, match, m.currentTime())
		if err != nil {
			return m.resourceAccessDecisionError(ctx, request, requestID, err), nil
		}
		feishuLog.Info(ctx, "denied feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s",
			shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
			request.ResourceType, shortResourceRef(request.ResourceToken))
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateDenied, "denied", "", "用户拒绝了本次资源授权。", m.currentTime())
		return approvalCallbackResponse(
			"success",
			"已拒绝，本次不会授予资源权限。",
			statusCard{title: "已拒绝授权", template: "grey", message: "请求已取消，未授予资源权限。"},
		), nil
	case resourceAccessCardActionSubmitOAuth:
		rawResult := resourceAccessCardOAuthResult(event)
		submission, err := m.parseResourceAccessOAuthSubmission(rawResult)
		if err != nil {
			feishuLog.Warn(ctx, "rejected feishu resource OAuth card input request=%s account=%s chars=%d: %v",
				shortRequestID(requestID), m.account.ID, len([]rune(rawResult)), err)
			return cardToast("error", err.Error()), nil
		}
		resultKind := "code"
		if strings.TrimSpace(submission.Response.Error) != "" {
			resultKind = "error"
		}
		feishuLog.Debug(ctx, "parsed feishu resource OAuth card handoff request=%s account=%s input_kind=%s input_length=%d callback_scheme=%s callback_host=%s callback_path=%s result_kind=%s state_ref=%s code_ref=%s code_length=%d oauth_error_ref=%s oauth_error_length=%d",
			shortRequestID(requestID), m.account.ID, submission.InputKind, submission.InputLength,
			submission.CallbackScheme, submission.CallbackHost, submission.CallbackPath, resultKind,
			shortResourceRef(submission.StateHash), shortResourceRef(submission.Response.Code), len(submission.Response.Code),
			shortResourceRef(submission.Response.Error), len(submission.Response.Error))
		request, err := m.store.ClaimFeishuResourceAccessOAuthFromCard(
			requestID,
			m.account.ID,
			submission.StateHash,
			match,
			m.currentTime(),
		)
		if err != nil {
			return m.resourceAccessDecisionError(ctx, request, requestID, err), nil
		}
		feishuLog.Info(ctx, "accepted feishu resource OAuth card handoff request=%s account=%s user=%s chat=%s input_kind=%s",
			shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID, submission.InputKind)
		go m.completeResourceAccessOAuthFromCard(request, submission.Response)
		return cardToast("success", "已收到授权结果，正在核验并授予资源权限；完成后机器人会更新卡片并通知当前对话。"), nil
	default:
		return cardToast("error", "不支持的资源授权操作。"), nil
	}
}

func (m *resourceAccessManager) completeApprovedResourceAccessFromCard(request store.FeishuResourceAccessRequest) {
	ctx, cancel := context.WithTimeout(m.baseContext(), resourceAccessCallbackTimeout)
	defer cancel()
	if err := m.completeApprovedResourceAccess(ctx, request); err != nil {
		m.finishResourceAccessFailure(
			ctx,
			request,
			err,
			"资源授权失败",
			"LingoBridge 未能完成本次资源授权，请重新调用资源授权工具。",
		)
	}
}

func (m *resourceAccessManager) completeApprovedResourceAccess(ctx context.Context, request store.FeishuResourceAccessRequest) error {
	current, err := m.store.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
	if err != nil {
		return fmt.Errorf("reload approved feishu resource access request: %w", err)
	}
	request = current
	if request.State != store.FeishuResourceAccessStatePending {
		return store.ErrFeishuResourceAccessResolved
	}
	if request.GrantMode != store.FeishuResourceGrantModeOnce && request.GrantMode != store.FeishuResourceGrantModeAll {
		return fmt.Errorf("approved feishu resource access request has no valid grant mode")
	}
	if request.SubjectType == "" || request.SubjectID == "" {
		return fmt.Errorf("approved feishu resource access request has no collaborator subject")
	}

	capability, active, err := m.store.ActiveFeishuResourceCapability(
		request.AccountID,
		request.ResourceType,
		request.ResourceToken,
		request.SubjectType,
		request.SubjectID,
		request.Permission,
	)
	if err != nil {
		return fmt.Errorf("load approved feishu resource capability: %w", err)
	}
	if active {
		verified, verifyErr := m.verifyTenantAccess(
			ctx,
			request.ResourceType,
			request.ResourceToken,
			request.Permission,
			capability.SubjectType,
			capability.SubjectID,
		)
		if verifyErr != nil {
			return fmt.Errorf("live-check approved feishu resource capability: %w", verifyErr)
		}
		if verified {
			capability.SourceRequestID = request.ID
			capability.VerifiedAt = m.currentTime()
			capability.UpdatedAt = capability.VerifiedAt
			return m.completeSelectedResourceGrant(ctx, request, capability, store.FeishuResourceGrantSourceExistingGrant)
		}
		m.revokeResourceCapabilityAndGrantBestEffort(ctx, request, capability.SubjectType, capability.SubjectID, "approved capability no longer verifies")
	}

	if !m.oauthEnabled() {
		return fmt.Errorf("feishu OAuth is unavailable and no verified resource capability exists")
	}
	accessToken, err := m.feishuUserAccessToken(ctx, request.ActorOpenID, request.ActorUserID)
	if err != nil {
		if errors.Is(err, ErrFeishuUserOAuthReauthorizationNeeded) {
			return m.prepareResourceAccessOAuthHandoff(ctx, request)
		}
		return fmt.Errorf("load persisted feishu user OAuth credential: %w", err)
	}
	return m.grantAndCompleteSelectedResourceAccess(ctx, request, accessToken)
}

func (m *resourceAccessManager) prepareResourceAccessOAuthHandoff(ctx context.Context, request store.FeishuResourceAccessRequest) error {
	if !m.oauthEnabled() {
		return fmt.Errorf("feishu OAuth callback is not configured")
	}
	state, stateHash, err := newResourceAccessOAuthValues()
	if err != nil {
		return fmt.Errorf("generate feishu resource OAuth state: %w", err)
	}
	if err := m.store.PrepareFeishuResourceAccessOAuth(
		request.ID,
		request.AccountID,
		stateHash,
		"",
		request.SubjectType,
		request.SubjectID,
		m.currentTime(),
	); err != nil {
		return fmt.Errorf("prepare feishu resource OAuth request: %w", err)
	}
	request.OAuthStateHash = stateHash
	authURL, err := m.authorizationURL(state)
	if err != nil {
		return err
	}
	authTrace := resourceAccessAuthorizationURLTrace(authURL)
	feishuLog.Debug(ctx, "prepared feishu resource OAuth request=%s account=%s grant_mode=%s pkce_mode=disabled state_ref=%s",
		shortRequestID(request.ID), request.AccountID, request.GrantMode, shortResourceRef(stateHash))
	feishuLog.Debug(ctx, "built feishu resource OAuth authorization URL request=%s account=%s pkce_mode=disabled valid=%t scheme=%s host=%s path=%s query_keys=%s url_length=%d auth_state_ref=%s state_matches=%t auth_code_challenge_present=%t auth_code_challenge_method_present=%t redirect_ref=%s redirect_matches=%t redirect_scheme=%s redirect_host=%s redirect_path=%s redirect_length=%d scope_count=%d",
		shortRequestID(request.ID), request.AccountID, authTrace.AuthorizationValid,
		authTrace.Scheme, authTrace.Host, authTrace.Path, authTrace.QueryKeys, authTrace.URLLength,
		authTrace.StateRef, authTrace.StateRef == shortResourceRef(stateHash),
		authTrace.CodeChallengePresent, authTrace.CodeChallengeMethodPresent,
		authTrace.RedirectRef, authTrace.RedirectRef == shortResourceRef(m.oauth.CallbackURL),
		authTrace.RedirectScheme, authTrace.RedirectHost, authTrace.RedirectPath, authTrace.RedirectLength, authTrace.ScopeCount)
	if err := m.cards.UpdateByMessageID(ctx, request.CardMessageID, pendingResourceAccessCard{request: request, authURL: authURL}); err != nil {
		return fmt.Errorf("update feishu resource authorization card for OAuth: %w", err)
	}
	feishuLog.Info(ctx, "requested feishu resource OAuth handoff request=%s account=%s user=%s chat=%s card_message=%s type=%s resource_ref=%s permission=%s grant_mode=%s state_ref=%s expires_at=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.CardMessageID, request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission,
		request.GrantMode, shortResourceRef(stateHash), request.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (m *resourceAccessManager) grantAndCompleteSelectedResourceAccess(ctx context.Context, request store.FeishuResourceAccessRequest, accessToken string) error {
	createCode, err := m.grantResourceAccess(ctx, accessToken, request)
	if err != nil && createCode != 1063003 {
		return err
	}
	verified, verifyErr := m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, request.Permission, request.SubjectType, request.SubjectID)
	if verifyErr != nil {
		return verifyErr
	}
	if !verified && createCode == 1063003 {
		if err := m.updateResourceAccess(ctx, accessToken, request); err != nil {
			return err
		}
		verified, verifyErr = m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, request.Permission, request.SubjectType, request.SubjectID)
		if verifyErr != nil {
			return verifyErr
		}
	}
	if !verified {
		return fmt.Errorf("feishu permission verification returned false")
	}
	completedAt := m.currentTime()
	capability := store.FeishuResourceCapability{
		AccountID:         request.AccountID,
		ResourceType:      request.ResourceType,
		ResourceToken:     request.ResourceToken,
		SubjectType:       request.SubjectType,
		SubjectID:         request.SubjectID,
		Permission:        request.Permission,
		SourceActorOpenID: request.ActorOpenID,
		SourceActorUserID: request.ActorUserID,
		SourceRequestID:   request.ID,
		State:             store.FeishuResourceCapabilityStateActive,
		CreatedAt:         completedAt,
		VerifiedAt:        completedAt,
	}
	return m.completeSelectedResourceGrant(ctx, request, capability, store.FeishuResourceGrantSourceNewlyGranted)
}

func (m *resourceAccessManager) completeSelectedResourceGrant(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	capability store.FeishuResourceCapability,
	source string,
) error {
	completedAt := m.currentTime()
	grantActorType, grantActorID, err := resourceAccessGrantActor(request.ActorOpenID, request.ActorUserID)
	if err != nil {
		return err
	}
	grant := store.FeishuResourceGrant{
		AccountID:       request.AccountID,
		ActorType:       grantActorType,
		ActorID:         grantActorID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      request.Permission,
		GrantMode:       request.GrantMode,
		SourceRequestID: request.ID,
		State:           store.FeishuResourceGrantStateActive,
		CreatedAt:       completedAt,
		UpdatedAt:       completedAt,
	}
	if request.GrantMode == store.FeishuResourceGrantModeOnce {
		grant.ExpiresAt = completedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute)
	}
	capability.SourceRequestID = request.ID
	capability.VerifiedAt = completedAt
	capability.UpdatedAt = completedAt
	if capability.CreatedAt.IsZero() {
		capability.CreatedAt = completedAt
	}
	if err := m.store.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		source,
		request.Permission,
		&capability,
		&grant,
		completedAt,
	); err != nil {
		return fmt.Errorf("save completed feishu resource authorization: %w", err)
	}
	request.UpdatedAt = completedAt
	m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateSucceeded, "granted", source, resourceAccessSuccessMessage(request, completedAt), completedAt)
	feishuLog.Info(ctx, "granted feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s grant_mode=%s once_minutes=%d subject_type=%s source=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission,
		request.GrantMode, request.OnceDurationMinutes, request.SubjectType, source)
	m.updateResourceAccessResultCard(ctx, request, statusCard{
		title:    "权限已授予",
		template: "green",
		message:  resourceAccessSuccessMessage(request, completedAt),
	})
	return nil
}

func resourceAccessSuccessMessage(request store.FeishuResourceAccessRequest, completedAt time.Time) string {
	if request.GrantMode == store.FeishuResourceGrantModeAll {
		return "飞书已确认所需资源权限；LingoBridge 已为当前用户、机器人账号、当前对话和这一精确资源保存永久授权。"
	}
	expiresAt := completedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute)
	return fmt.Sprintf("飞书已确认所需资源权限；LingoBridge 将允许当前范围使用该权限 %d 分钟，至 %s。到期后不会撤销飞书协作者。",
		request.OnceDurationMinutes, expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}

type resourceAccessOAuthResponse struct {
	Code  string
	Error string
}

type resourceAccessOAuthSubmission struct {
	InputKind      string
	InputLength    int
	CallbackScheme string
	CallbackHost   string
	CallbackPath   string
	StateHash      string
	Response       resourceAccessOAuthResponse
}

type resourceAccessOAuthQuery struct {
	State    string
	Response resourceAccessOAuthResponse
}

type resourceAccessAuthorizationTrace struct {
	Scheme                     string
	Host                       string
	Path                       string
	QueryKeys                  string
	URLLength                  int
	StateRef                   string
	CodeChallengePresent       bool
	CodeChallengeMethodPresent bool
	RedirectRef                string
	RedirectScheme             string
	RedirectHost               string
	RedirectPath               string
	RedirectLength             int
	ScopeCount                 int
	AuthorizationValid         bool
}

type resourceAccessOAuthCompletionError struct {
	cause        error
	httpStatus   int
	httpResponse string
}

func (e *resourceAccessOAuthCompletionError) Error() string {
	if e == nil || e.cause == nil {
		return "feishu resource OAuth completion failed"
	}
	return e.cause.Error()
}

func (e *resourceAccessOAuthCompletionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (m *resourceAccessManager) parseResourceAccessOAuthSubmission(raw string) (resourceAccessOAuthSubmission, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resourceAccessOAuthSubmission{}, fmt.Errorf("请粘贴完整回调 URL 或授权码。")
	}
	if len([]rune(raw)) > resourceAccessOAuthResultMaxLength {
		return resourceAccessOAuthSubmission{}, fmt.Errorf("授权结果超过 %d 个字符，请重新复制。", resourceAccessOAuthResultMaxLength)
	}
	if strings.Contains(raw, "://") {
		submitted, err := url.ParseRequestURI(raw)
		if err != nil || submitted.Scheme == "" || submitted.Host == "" || submitted.User != nil || submitted.Fragment != "" {
			return resourceAccessOAuthSubmission{}, fmt.Errorf("授权回调 URL 格式无效，请复制浏览器地址栏中的完整 URL。")
		}
		configured, err := url.Parse(m.oauth.CallbackURL)
		if err != nil || configured.Scheme == "" || configured.Host == "" {
			return resourceAccessOAuthSubmission{}, fmt.Errorf("机器人 OAuth 回调配置无效，请联系管理员。")
		}
		if !strings.EqualFold(submitted.Scheme, configured.Scheme) ||
			!strings.EqualFold(submitted.Host, configured.Host) ||
			normalizedOAuthCallbackPath(submitted) != normalizedOAuthCallbackPath(configured) {
			return resourceAccessOAuthSubmission{}, fmt.Errorf("授权回调 URL 与当前机器人配置不匹配。")
		}
		query, err := parseResourceAccessOAuthQuery(submitted.RawQuery)
		if err != nil {
			return resourceAccessOAuthSubmission{}, err
		}
		return resourceAccessOAuthSubmission{
			InputKind:      "url",
			InputLength:    len(raw),
			CallbackScheme: submitted.Scheme,
			CallbackHost:   submitted.Host,
			CallbackPath:   normalizedOAuthCallbackPath(submitted),
			StateHash:      hashResourceAccessState(query.State),
			Response:       query.Response,
		}, nil
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return resourceAccessOAuthSubmission{}, fmt.Errorf("授权码不能包含空白字符，请重新复制。")
	}
	return resourceAccessOAuthSubmission{
		InputKind:   "code",
		InputLength: len(raw),
		Response:    resourceAccessOAuthResponse{Code: raw},
	}, nil
}

func resourceAccessAuthorizationURLTrace(raw string) resourceAccessAuthorizationTrace {
	trace := resourceAccessAuthorizationTrace{URLLength: len(raw)}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trace
	}
	trace.AuthorizationValid = true
	trace.Scheme = parsed.Scheme
	trace.Host = parsed.Host
	trace.Path = normalizedOAuthCallbackPath(parsed)
	query := parsed.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	trace.QueryKeys = strings.Join(keys, ",")
	if state := query.Get("state"); state != "" {
		trace.StateRef = shortResourceRef(hashResourceAccessState(state))
	}
	trace.CodeChallengePresent = query.Has("code_challenge")
	trace.CodeChallengeMethodPresent = query.Has("code_challenge_method")
	trace.ScopeCount = len(strings.Fields(query.Get("scope")))
	redirectRaw := query.Get("redirect_uri")
	trace.RedirectRef = shortResourceRef(redirectRaw)
	trace.RedirectLength = len(redirectRaw)
	if redirect, err := url.Parse(redirectRaw); err == nil {
		trace.RedirectScheme = redirect.Scheme
		trace.RedirectHost = redirect.Host
		trace.RedirectPath = normalizedOAuthCallbackPath(redirect)
	}
	return trace
}

func parseResourceAccessOAuthQuery(rawQuery string) (resourceAccessOAuthQuery, error) {
	values := map[string][]string{
		"state": nil,
		"code":  nil,
		"error": nil,
	}
	for _, field := range strings.Split(rawQuery, "&") {
		if field == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(field, "=")
		key, err := url.PathUnescape(rawKey)
		if err != nil {
			return resourceAccessOAuthQuery{}, fmt.Errorf("授权回调 URL 包含无效的查询参数编码。")
		}
		if _, tracked := values[key]; !tracked {
			continue
		}
		value, err := url.PathUnescape(rawValue)
		if err != nil {
			return resourceAccessOAuthQuery{}, fmt.Errorf("授权回调 URL 包含无效的查询参数编码。")
		}
		values[key] = append(values[key], value)
	}
	if len(values["state"]) != 1 || strings.TrimSpace(values["state"][0]) == "" {
		return resourceAccessOAuthQuery{}, fmt.Errorf("授权回调 URL 缺少有效的 state，请重新发起授权。")
	}
	if len(values["code"]) > 1 || len(values["error"]) > 1 {
		return resourceAccessOAuthQuery{}, fmt.Errorf("授权回调 URL 包含重复的授权结果参数，请重新发起授权。")
	}
	code := firstResourceAccessOAuthQueryValue(values["code"])
	oauthError := firstResourceAccessOAuthQueryValue(values["error"])
	if (strings.TrimSpace(code) == "") == (strings.TrimSpace(oauthError) == "") {
		return resourceAccessOAuthQuery{}, fmt.Errorf("授权回调 URL 必须且只能包含授权码或 OAuth 错误。")
	}
	return resourceAccessOAuthQuery{
		State: strings.TrimSpace(values["state"][0]),
		Response: resourceAccessOAuthResponse{
			Code:  code,
			Error: oauthError,
		},
	}, nil
}

func firstResourceAccessOAuthQueryValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func normalizedOAuthCallbackPath(value *url.URL) string {
	if value == nil {
		return ""
	}
	path := value.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func (m *resourceAccessManager) completeResourceAccessOAuthFromCard(request store.FeishuResourceAccessRequest, response resourceAccessOAuthResponse) {
	ctx, cancel := context.WithTimeout(m.baseContext(), resourceAccessCallbackTimeout)
	defer cancel()
	if err := m.completeResourceAccessOAuth(ctx, request, response); err != nil {
		return
	}
}

func (m *resourceAccessManager) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query, err := parseResourceAccessOAuthQuery(r.URL.RawQuery)
	if err != nil {
		feishuLog.Warn(r.Context(), "reject malformed feishu resource OAuth callback account=%s: %v", m.account.ID, err)
		http.Error(w, "invalid OAuth callback query", http.StatusBadRequest)
		return
	}
	resultKind := "code"
	if strings.TrimSpace(query.Response.Error) != "" {
		resultKind = "error"
	}
	feishuLog.Debug(r.Context(), "parsed feishu resource OAuth HTTP callback account=%s callback_host=%s callback_path=%s raw_query_length=%d result_kind=%s state_ref=%s code_ref=%s code_length=%d oauth_error_ref=%s oauth_error_length=%d",
		m.account.ID, r.Host, normalizedOAuthCallbackPath(r.URL), len(r.URL.RawQuery), resultKind,
		shortResourceRef(hashResourceAccessState(query.State)), shortResourceRef(query.Response.Code), len(query.Response.Code),
		shortResourceRef(query.Response.Error), len(query.Response.Error))
	request, err := m.store.ClaimFeishuResourceAccessOAuth(hashResourceAccessState(query.State), m.account.ID, m.currentTime())
	if err != nil {
		if errors.Is(err, store.ErrFeishuResourceAccessExpired) {
			m.persistResourceWorkflowResult(r.Context(), request, store.WorkflowResultStateExpired, "expired", "", "资源授权请求已过期。", m.currentTime())
		}
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrFeishuResourceAccessExpired) {
			status = http.StatusGone
		}
		feishuLog.Warn(r.Context(), "reject feishu resource OAuth callback account=%s state_ref=%s: %v", m.account.ID, shortResourceRef(hashResourceAccessState(query.State)), err)
		http.Error(w, "OAuth request is invalid, expired, or already used", status)
		return
	}
	callbackCtx, cancel := context.WithTimeout(m.baseContext(), resourceAccessCallbackTimeout)
	defer cancel()
	err = m.completeResourceAccessOAuth(callbackCtx, request, query.Response)
	if err != nil {
		var completionErr *resourceAccessOAuthCompletionError
		if errors.As(err, &completionErr) {
			http.Error(w, completionErr.httpResponse, completionErr.httpStatus)
			return
		}
		http.Error(w, "resource access authorization failed", http.StatusInternalServerError)
		return
	}
	redirectToResource(w, r, request.ResourceURL)
}

func (m *resourceAccessManager) completeResourceAccessOAuth(ctx context.Context, request store.FeishuResourceAccessRequest, response resourceAccessOAuthResponse) error {
	fail := func(cause error, title, message string, httpStatus int, httpResponse string) error {
		m.finishOAuthFailure(ctx, request, cause, title, message)
		return &resourceAccessOAuthCompletionError{
			cause:        cause,
			httpStatus:   httpStatus,
			httpResponse: httpResponse,
		}
	}

	if strings.TrimSpace(response.Error) != "" {
		return fail(
			fmt.Errorf("user did not complete feishu authorization"),
			"授权未完成",
			"用户未在飞书官方授权页完成授权。",
			http.StatusForbidden,
			"Feishu authorization was not completed",
		)
	}
	code := strings.TrimSpace(response.Code)
	if code == "" {
		return fail(
			fmt.Errorf("missing authorization code"),
			"授权失败",
			"飞书回调缺少授权码，请重新发起。",
			http.StatusBadRequest,
			"missing authorization code",
		)
	}
	if strings.TrimSpace(request.PKCEVerifier) != "" {
		err := fmt.Errorf("legacy PKCE-enabled feishu resource OAuth request")
		message := fmt.Sprintf("此授权卡片由旧版本创建，已失效。请重新调用 `%s`，并只使用最新授权卡片。", feishutools.ResourceAccessToolName)
		return fail(
			err,
			"授权请求已失效",
			message,
			http.StatusBadRequest,
			"legacy PKCE authorization request is no longer supported; start a new request",
		)
	}
	tokens, err := m.exchangeAuthorizationCode(ctx, request.ID, code)
	if err != nil {
		m.logResourceAccessTokenExchangeError(ctx, request.ID, code, err)
		return fail(err, "授权失败", "飞书授权码兑换失败，请重新发起。", http.StatusBadGateway, "authorization code exchange failed")
	}
	identity, err := m.verifyOAuthUser(ctx, tokens.AccessToken, request)
	if err != nil {
		return fail(err, "授权用户不匹配", "只有发起资源请求的飞书用户可以完成授权。", http.StatusForbidden, "authorized user does not match the requester")
	}
	if _, err := m.persistFeishuOAuthCredential(ctx, identity, tokens); err != nil {
		return fail(err, "授权凭证保存失败", "LingoBridge 未能安全保存飞书授权凭证，请重新发起。", http.StatusInternalServerError, "saving encrypted OAuth credential failed")
	}
	if err := m.grantAndCompleteSelectedResourceAccess(ctx, request, tokens.AccessToken); err != nil {
		return fail(err, "授予权限失败", "LingoBridge 未能完成飞书资源权限授予，请重新发起资源授权。", http.StatusBadGateway, "granting resource access failed")
	}
	return nil
}

func (m *resourceAccessManager) logResourceAccessTokenExchangeError(
	ctx context.Context,
	requestID, code string,
	err error,
) {
	var accessTokenErr *accesstoken.AccessTokenError
	if !errors.As(err, &accessTokenErr) {
		feishuLog.Warn(ctx, "feishu resource OAuth token request transport failure request=%s account=%s pkce_mode=disabled sdk_code_verifier_present=false error_go_type=%T code_ref=%s code_length=%d",
			shortRequestID(requestID), m.account.ID, err, shortResourceRef(code), len(code))
		return
	}
	httpStatus := 0
	requestLogID := ""
	responseBytes := 0
	contentType := ""
	if accessTokenErr.ApiResp != nil {
		httpStatus = accessTokenErr.ApiResp.StatusCode
		requestLogID = accessTokenErr.ApiResp.RequestId()
		responseBytes = len(accessTokenErr.ApiResp.RawBody)
		contentType = accessTokenErr.ApiResp.Header.Get("Content-Type")
	}
	feishuLog.Warn(ctx, "feishu resource OAuth token error response request=%s account=%s pkce_mode=disabled sdk_code_verifier_present=false oauth_service_inconsistency=%t http_status=%d feishu_code=%d oauth_error_type=%q oauth_error_description_ref=%s oauth_error_description_length=%d request_log_id=%q content_type=%q response_bytes=%d code_ref=%s code_length=%d",
		shortRequestID(requestID), m.account.ID, accessTokenErr.Code == 20049, httpStatus, accessTokenErr.Code, accessTokenErr.ErrorType,
		shortResourceRef(accessTokenErr.ErrorDescription), len(accessTokenErr.ErrorDescription), requestLogID, contentType, responseBytes,
		shortResourceRef(code), len(code))
}

func (m *resourceAccessManager) exchangeAuthorizationCode(ctx context.Context, requestID, code string) (feishuOAuthTokenBundle, error) {
	tokenRequest := authorizationcode.NewTokenRequestBuilder().
		Code(code).
		RedirectUri(m.oauth.CallbackURL).
		Build()
	sdkCode := ""
	sdkRedirect := ""
	sdkCodeVerifierPresent := false
	if tokenRequest != nil && tokenRequest.Body != nil {
		sdkCode = deref(tokenRequest.Body.Code)
		sdkRedirect = deref(tokenRequest.Body.RedirectUri)
		sdkCodeVerifierPresent = tokenRequest.Body.CodeVerifier != nil
	}
	endpoint, _ := url.Parse(strings.TrimRight(m.oauth.BaseURL, "/") + larkcore.OAuthTokenUrlPath)
	redirect, _ := url.Parse(m.oauth.CallbackURL)
	feishuLog.Debug(ctx, "prepared feishu resource OAuth token request request=%s account=%s pkce_mode=disabled endpoint_scheme=%s endpoint_host=%s endpoint_path=%s grant_type=authorization_code content_type=application/json code_ref=%s code_length=%d sdk_code_ref=%s sdk_code_length=%d sdk_code_matches=%t sdk_code_verifier_present=%t redirect_ref=%s redirect_scheme=%s redirect_host=%s redirect_path=%s redirect_length=%d sdk_redirect_ref=%s sdk_redirect_length=%d sdk_redirect_matches=%t",
		shortRequestID(requestID), m.account.ID, endpoint.Scheme, endpoint.Host, normalizedOAuthCallbackPath(endpoint),
		shortResourceRef(code), len(code), shortResourceRef(sdkCode), len(sdkCode), sdkCode == code,
		sdkCodeVerifierPresent,
		shortResourceRef(m.oauth.CallbackURL), redirect.Scheme, redirect.Host, normalizedOAuthCallbackPath(redirect), len(m.oauth.CallbackURL),
		shortResourceRef(sdkRedirect), len(sdkRedirect), sdkRedirect == m.oauth.CallbackURL)
	resp, err := m.client.AccessToken.RetrieveByAuthorizationCode(ctx, tokenRequest)
	if err != nil {
		return feishuOAuthTokenBundle{}, fmt.Errorf("exchange feishu OAuth authorization code: %w", err)
	}
	bundle, err := feishuOAuthTokenBundleFromResponse(resp, resourceAccessOAuthScope)
	if err != nil {
		return feishuOAuthTokenBundle{}, fmt.Errorf("exchange feishu OAuth authorization code: %w", err)
	}
	statusCode := 0
	requestLogID := ""
	if resp.ApiResp != nil {
		statusCode = resp.ApiResp.StatusCode
		requestLogID = resp.ApiResp.RequestId()
	}
	scopeCount := 0
	expiresIn := 0
	refreshTokenPresent := false
	refreshExpiresIn := 0
	if resp.Data.Scope != nil {
		scopeCount = len(strings.Fields(deref(resp.Data.Scope)))
	}
	if resp.Data.ExpiresIn != nil {
		expiresIn = *resp.Data.ExpiresIn
	}
	refreshTokenPresent = strings.TrimSpace(deref(resp.Data.RefreshToken)) != ""
	if resp.Data.RefreshTokenExpiresIn != nil {
		refreshExpiresIn = *resp.Data.RefreshTokenExpiresIn
	}
	feishuLog.Debug(ctx, "received feishu resource OAuth token response request=%s account=%s http_status=%d request_log_id=%q access_token_present=true refresh_token_present=%t expires_in=%d refresh_expires_in=%d scope_count=%d",
		shortRequestID(requestID), m.account.ID, statusCode, requestLogID, refreshTokenPresent, expiresIn, refreshExpiresIn, scopeCount)
	return bundle, nil
}

func (m *resourceAccessManager) verifyOAuthUser(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) (feishuOAuthIdentity, error) {
	resp, err := m.client.Authen.UserInfo.Get(ctx, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return feishuOAuthIdentity{}, fmt.Errorf("get authorized feishu user: %w", err)
	}
	if resp == nil || !resp.Success() || resp.Data == nil {
		if resp == nil {
			return feishuOAuthIdentity{}, fmt.Errorf("get authorized feishu user: empty response")
		}
		return feishuOAuthIdentity{}, fmt.Errorf("get authorized feishu user code=%d msg=%s", resp.Code, resp.Msg)
	}
	identity := feishuOAuthIdentity{
		OpenID: strings.TrimSpace(deref(resp.Data.OpenId)),
		UserID: strings.TrimSpace(deref(resp.Data.UserId)),
	}
	if request.ActorOpenID != "" {
		if request.ActorOpenID != identity.OpenID {
			return feishuOAuthIdentity{}, fmt.Errorf("authorized open_id does not match requester")
		}
		return identity, nil
	}
	if request.ActorUserID == "" || request.ActorUserID != identity.UserID {
		return feishuOAuthIdentity{}, fmt.Errorf("authorized user_id does not match requester")
	}
	return identity, nil
}

func (m *resourceAccessManager) grantResourceAccess(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) (int, error) {
	perm := larkdrive.PermCreatePermissionMemberView
	if request.Permission == store.FeishuResourcePermissionWrite {
		perm = larkdrive.PermCreatePermissionMemberEdit
		if request.ResourceType == "folder" && request.SubjectType == larkdrive.MemberTypeOpenChat {
			perm = larkdrive.PermCreatePermissionMemberFullAccess
		}
	}
	memberKind := larkdrive.TypeCreatePermissionMemberUser
	if request.SubjectType == larkdrive.MemberTypeOpenChat {
		memberKind = larkdrive.TypeCreatePermissionMemberChat
	}
	req := larkdrive.NewCreatePermissionMemberReqBuilder().
		Token(request.ResourceToken).
		Type(request.ResourceType).
		NeedNotification(false).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(request.SubjectType).
			MemberId(request.SubjectID).
			Perm(perm).
			Type(memberKind).
			Build()).
		Build()
	resp, err := m.client.Drive.PermissionMember.Create(ctx, req, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return 0, fmt.Errorf("grant feishu resource permission: %w", err)
	}
	if resp == nil {
		return 0, fmt.Errorf("grant feishu resource permission: empty response")
	}
	if !resp.Success() {
		return resp.Code, fmt.Errorf("grant feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Code, nil
}

func (m *resourceAccessManager) updateResourceAccess(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) error {
	perm := larkdrive.PermUpdatePermissionMemberView
	if request.Permission == store.FeishuResourcePermissionWrite {
		perm = larkdrive.PermUpdatePermissionMemberEdit
		if request.ResourceType == "folder" && request.SubjectType == larkdrive.MemberTypeOpenChat {
			perm = larkdrive.PermUpdatePermissionMemberFullAccess
		}
	}
	memberKind := larkdrive.TypeUpdatePermissionMemberUser
	if request.SubjectType == larkdrive.MemberTypeOpenChat {
		memberKind = larkdrive.TypeUpdatePermissionMemberChat
	}
	req := larkdrive.NewUpdatePermissionMemberReqBuilder().
		Token(request.ResourceToken).
		MemberId(request.SubjectID).
		Type(request.ResourceType).
		NeedNotification(false).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(request.SubjectType).
			MemberId(request.SubjectID).
			Perm(perm).
			Type(memberKind).
			Build()).
		Build()
	resp, err := m.client.Drive.PermissionMember.Update(ctx, req, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return fmt.Errorf("update feishu resource permission: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("update feishu resource permission: empty response")
	}
	if !resp.Success() {
		return fmt.Errorf("update feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (m *resourceAccessManager) verifyTenantAccess(ctx context.Context, resourceType, resourceToken, permission, subjectType, subjectID string) (bool, error) {
	if resourceType == "folder" {
		resp, err := m.client.Drive.PermissionMember.List(ctx, larkdrive.NewListPermissionMemberReqBuilder().
			Token(resourceToken).
			Type(resourceType).
			Build())
		if err != nil {
			return false, fmt.Errorf("list feishu folder collaborators: %w", err)
		}
		if resp == nil || !resp.Success() {
			if resp == nil {
				return false, fmt.Errorf("list feishu folder collaborators: empty response")
			}
			return false, fmt.Errorf("list feishu folder collaborators code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data == nil {
			return false, nil
		}
		for _, member := range resp.Data.Items {
			if member == nil || strings.TrimSpace(deref(member.MemberType)) != subjectType || strings.TrimSpace(deref(member.MemberId)) != subjectID {
				continue
			}
			return feishuCollaboratorPermissionSatisfies(strings.TrimSpace(deref(member.Perm)), permission, true), nil
		}
		return false, nil
	}
	action := larkdrive.PermView
	if permission == store.FeishuResourcePermissionWrite {
		action = larkdrive.PermEdit
	}
	resp, err := m.client.Drive.PermissionMember.Auth(ctx, larkdrive.NewAuthPermissionMemberReqBuilder().
		Token(resourceToken).
		Type(resourceType).
		Action(action).
		Build())
	if err != nil {
		return false, fmt.Errorf("verify feishu resource permission: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return false, fmt.Errorf("verify feishu resource permission: empty response")
		}
		return false, fmt.Errorf("verify feishu resource permission code=%d msg=%s", resp.Code, resp.Msg)
	}
	return resp.Data != nil && resp.Data.AuthResult != nil && *resp.Data.AuthResult, nil
}

func (m *resourceAccessManager) resolveResource(ctx context.Context, chat feishutools.ChatContext, input feishutools.ResourceAccessRequest) (string, string, string, error) {
	resourceType := feishutools.NormalizeResourceType(input.ResourceType)
	resourceToken := strings.TrimSpace(input.ResourceToken)
	resourceURL := strings.TrimSpace(input.ResourceURL)
	switch resourceToken {
	case feishutools.BotRootResourceAlias:
		root, err := m.applicationRootFolder(ctx)
		if err != nil {
			return "", "", "", err
		}
		resourceToken = root.Token
		if resourceURL == "" {
			resourceURL = defaultFeishuResourceURL("folder", resourceToken)
		}
		if _, err := m.store.SaveFeishuBotResource(store.FeishuBotResource{
			AccountID:     m.account.ID,
			ResourceType:  "folder",
			ResourceToken: resourceToken,
			Name:          "Bot Root",
			URL:           resourceURL,
			CreatedAt:     m.currentTime(),
		}); err != nil {
			return "", "", "", fmt.Errorf("record feishu Bot root ownership: %w", err)
		}
	case feishutools.ChatDefaultFolderResourceAlias:
		folder, err := m.store.DefaultFeishuChatFolder(m.account.ID, chat.ChatID)
		if err != nil {
			if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
				return "", "", "", fmt.Errorf("current Feishu chat has no default Bot folder; create one first")
			}
			return "", "", "", fmt.Errorf("resolve current chat default folder: %w", err)
		}
		resourceToken = folder.FolderToken
		if resourceURL == "" {
			resourceURL = folder.URL
		}
		if _, err := m.store.SaveFeishuBotResource(store.FeishuBotResource{
			AccountID:       m.account.ID,
			ResourceType:    "folder",
			ResourceToken:   folder.FolderToken,
			ParentToken:     folder.ParentFolderToken,
			Name:            folder.Name,
			URL:             folder.URL,
			SourceRequestID: folder.CreateRequestID,
			CreatedAt:       folder.CreatedAt,
		}); err != nil {
			return "", "", "", fmt.Errorf("record current chat Bot folder ownership: %w", err)
		}
	}
	if resourceURL == "" {
		resourceURL = defaultFeishuResourceURL(resourceType, resourceToken)
	}
	return resourceType, resourceToken, resourceURL, nil
}

type resourceAccessRootFolder struct {
	Token string
}

func (m *resourceAccessManager) applicationRootFolder(ctx context.Context) (resourceAccessRootFolder, error) {
	resp, err := m.client.Get(ctx, "/open-apis/drive/explorer/v2/root_folder/meta", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder: %w", err)
	}
	if resp == nil {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder: empty response")
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return resourceAccessRootFolder{}, fmt.Errorf("parse feishu application root folder: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	result.Data.Token = strings.TrimSpace(result.Data.Token)
	if result.Data.Token == "" {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder returned no token")
	}
	return resourceAccessRootFolder{Token: result.Data.Token}, nil
}

func (m *resourceAccessManager) resourceGrantSubject(chat feishutools.ChatContext, resourceType string) (string, string, string) {
	if resourceType == "folder" {
		if !chat.IsGroup {
			return "", "", "飞书不支持把应用机器人直接添加为私聊外部文件夹的协作者。请使用 Bot 自有目录，或在群聊中给当前群组授予该文件夹权限。"
		}
		return larkdrive.MemberTypeOpenChat, chat.ChatID, ""
	}
	return larkdrive.MemberTypeOpenId, m.botOpenID, ""
}

func (m *resourceAccessManager) authorizationURL(state string) (string, error) {
	if !m.oauthEnabled() {
		return "", fmt.Errorf("feishu OAuth callback is not configured")
	}
	base, err := url.Parse(m.oauth.BaseURL + "/open-apis/authen/v1/authorize")
	if err != nil {
		return "", fmt.Errorf("build feishu OAuth authorization URL: %w", err)
	}
	query := base.Query()
	query.Set("client_id", m.oauth.ClientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", m.oauth.CallbackURL)
	query.Set("scope", resourceAccessOAuthScope)
	query.Set("state", state)
	query.Set("prompt", "consent")
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func (m *resourceAccessManager) oauthEnabled() bool {
	return m.oauth.ClientID != "" && m.oauth.BaseURL != "" && m.oauth.CallbackURL != ""
}

func (m *resourceAccessManager) oauthHTTPCallbackEnabled() bool {
	return m.oauthEnabled() && m.oauth.CallbackListenAddress != ""
}

func (m *resourceAccessManager) resourceAccessResult(request store.FeishuResourceAccessRequest, status, source string) feishutools.ResourceAccessResult {
	return feishutools.ResourceAccessResult{
		RequestID:     request.ID,
		Status:        status,
		Permission:    request.Permission,
		Source:        source,
		ResourceType:  request.ResourceType,
		ResourceToken: request.ResourceToken,
		ResourceURL:   request.ResourceURL,
	}
}

func (m *resourceAccessManager) resourceAccessDecisionError(ctx context.Context, request store.FeishuResourceAccessRequest, requestID string, err error) *callback.CardActionTriggerResponse {
	switch {
	case errors.Is(err, store.ErrFeishuResourceAccessForbidden):
		return cardToast("error", "只有发起请求的用户可以处理该授权。")
	case errors.Is(err, store.ErrFeishuResourceAccessContextMismatch):
		return cardToast("error", "授权卡片与原请求不匹配。")
	case errors.Is(err, store.ErrFeishuResourceAccessOAuthStateMismatch):
		return cardToast("error", "授权链接的 state 与原请求不匹配，请重新复制或重新发起授权。")
	case errors.Is(err, store.ErrFeishuResourceAccessExpired):
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateExpired, "expired", "", "资源授权请求已过期。", m.currentTime())
		return approvalCallbackResponse("error", "授权请求已过期。", statusCard{title: "授权已过期", template: "grey", message: "请重新调用资源授权工具。"})
	case errors.Is(err, store.ErrFeishuResourceAccessResolved):
		return cardToast("info", "该授权请求已经处理。")
	case errors.Is(err, store.ErrFeishuResourceAccessNotFound):
		return cardToast("error", "授权请求不存在或已失效。")
	default:
		feishuLog.Error(ctx, "handle feishu resource access card failed request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
		return cardToast("error", "处理授权失败，请稍后重试。")
	}
}

func (m *resourceAccessManager) finishResourceAccessFailure(ctx context.Context, request store.FeishuResourceAccessRequest, cause error, title, message string) {
	if errors.Is(cause, store.ErrFeishuResourceAccessResolved) {
		return
	}
	failedAt := m.currentTime()
	if err := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, failedAt); err != nil {
		if errors.Is(err, store.ErrFeishuResourceAccessResolved) {
			return
		}
		feishuLog.Error(ctx, "mark feishu resource authorization failed request=%s account=%s: %v", shortRequestID(request.ID), request.AccountID, err)
	} else {
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateFailed, "failed", "", message, failedAt)
	}
	feishuLog.Warn(ctx, "feishu resource authorization failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s grant_mode=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.GrantMode, cause)
	m.updateResourceAccessResultCard(ctx, request, statusCard{title: title, template: "red", message: message})
}

func (m *resourceAccessManager) finishOAuthFailure(ctx context.Context, request store.FeishuResourceAccessRequest, cause error, title, message string) {
	failedAt := m.currentTime()
	if err := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, failedAt); err != nil && !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
		feishuLog.Error(ctx, "mark feishu resource OAuth failed request=%s account=%s: %v", shortRequestID(request.ID), request.AccountID, err)
	} else if err == nil {
		m.persistResourceWorkflowResult(ctx, request, store.WorkflowResultStateFailed, "failed", "", message, failedAt)
	}
	feishuLog.Warn(ctx, "feishu resource OAuth failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), cause)
	m.updateResourceAccessResultCard(ctx, request, statusCard{title: title, template: "red", message: message})
}

func (m *resourceAccessManager) persistResourceWorkflowResult(ctx context.Context, request store.FeishuResourceAccessRequest, state, status, source, message string, now time.Time) {
	if strings.TrimSpace(request.ID) == "" {
		return
	}
	persistWorkflowResultBestEffort(ctx, m.store, request.ID, request.AccountID, state, resourceWorkflowResultPayload(request, status, source, message), now)
}

func resourceWorkflowResultPayload(request store.FeishuResourceAccessRequest, status, source, message string) map[string]any {
	payload := map[string]any{
		"status":         status,
		"permission":     request.Permission,
		"source":         source,
		"resource_type":  request.ResourceType,
		"resource_token": request.ResourceToken,
		"resource_url":   request.ResourceURL,
		"message":        strings.TrimSpace(message),
	}
	if request.GrantMode != "" {
		payload["grant_mode"] = request.GrantMode
		payload["once_duration_minutes"] = request.OnceDurationMinutes
		if request.GrantMode == store.FeishuResourceGrantModeOnce && !request.UpdatedAt.IsZero() && status == "granted" {
			payload["access_expires_at"] = request.UpdatedAt.Add(time.Duration(request.OnceDurationMinutes) * time.Minute).UTC().Format(time.RFC3339)
		}
	}
	return payload
}

func (m *resourceAccessManager) reconcileTerminalResourceAccessResults(ctx context.Context, updatedBefore time.Time) (int, error) {
	const batchSize = 100
	total := 0
	for {
		gaps, err := m.store.ListTerminalWorkflowResultGaps(
			m.account.ID,
			store.WorkflowRequestKindFeishuResourceAccess,
			updatedBefore,
			batchSize,
		)
		if err != nil {
			return total, err
		}
		for _, gap := range gaps {
			request, err := m.store.GetFeishuResourceAccessRequest(gap.ID, gap.AccountID)
			if err != nil {
				return total, fmt.Errorf("load terminal resource access request %s: %w", shortRequestID(gap.ID), err)
			}
			resultState, status, source, message, err := recoveredResourceAccessResult(request, gap.State)
			if err != nil {
				return total, err
			}
			_, ready, err := persistWorkflowResult(
				m.store,
				request.ID,
				request.AccountID,
				resultState,
				resourceWorkflowResultPayload(request, status, source, message),
				gap.UpdatedAt,
			)
			if err != nil {
				return total, fmt.Errorf("store recovered resource access result %s: %w", shortRequestID(gap.ID), err)
			}
			total++
			feishuLog.Debug(ctx, "reconciled feishu resource access result request=%s account=%s workflow_state=%s result_state=%s ready=%t",
				shortRequestID(gap.ID), gap.AccountID, gap.State, resultState, ready)
		}
		if len(gaps) < batchSize {
			return total, nil
		}
	}
}

func recoveredResourceAccessResult(request store.FeishuResourceAccessRequest, workflowState string) (resultState, status, source, message string, err error) {
	switch workflowState {
	case store.WorkflowRequestStateDenied:
		return store.WorkflowResultStateDenied, "denied", "", "用户拒绝了本次资源授权。", nil
	case store.WorkflowRequestStateExpired:
		return store.WorkflowResultStateExpired, "expired", "", "资源授权请求已过期。", nil
	case store.WorkflowRequestStateFailed:
		return store.WorkflowResultStateFailed, "failed", "", "资源授权未能完成。", nil
	case store.WorkflowRequestStateSucceeded:
		source := strings.TrimSpace(request.GrantSource)
		return store.WorkflowResultStateSucceeded, "granted", source, "飞书已确认所需资源权限。", nil
	default:
		return "", "", "", "", fmt.Errorf("unsupported terminal resource access state %q", workflowState)
	}
}

func (m *resourceAccessManager) updateResourceAccessResultCard(ctx context.Context, request store.FeishuResourceAccessRequest, card Card) {
	updateCtx, cancel := context.WithTimeout(ctx, resourceAccessCardUpdateTimeout)
	defer cancel()
	m.updateResourceCardBestEffort(updateCtx, request.CardMessageID, card)
}

func (m *resourceAccessManager) updateResourceCardBestEffort(ctx context.Context, messageID string, card Card) {
	if strings.TrimSpace(messageID) == "" || card == nil {
		return
	}
	if err := m.cards.UpdateByMessageID(ctx, messageID, card); err != nil {
		feishuLog.Warn(ctx, "update feishu resource access card failed message=%s: %v", messageID, err)
	}
}

func (m *resourceAccessManager) revokeResourceCapabilityAndGrantBestEffort(
	ctx context.Context,
	request store.FeishuResourceAccessRequest,
	subjectType, subjectID, reason string,
) {
	now := m.currentTime()
	if strings.TrimSpace(subjectType) != "" && strings.TrimSpace(subjectID) != "" {
		if err := m.store.RevokeFeishuResourceCapability(
			request.AccountID,
			request.ResourceType,
			request.ResourceToken,
			subjectType,
			subjectID,
			now,
		); err != nil && !errors.Is(err, store.ErrFeishuResourceCapabilityNotFound) {
			feishuLog.Warn(ctx, "revoke feishu resource capability failed request=%s account=%s chat=%s type=%s resource_ref=%s reason=%q: %v",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
				shortResourceRef(request.ResourceToken), reason, err)
		}
	}
	if err := m.store.RevokeFeishuResourceGrant(
		request.AccountID,
		request.ChatID,
		request.ResourceType,
		request.ResourceToken,
		now,
	); err != nil && !errors.Is(err, store.ErrFeishuResourceGrantNotFound) {
		feishuLog.Warn(ctx, "revoke feishu resource grant failed request=%s account=%s chat=%s type=%s resource_ref=%s reason=%q: %v",
			shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType,
			shortResourceRef(request.ResourceToken), reason, err)
	}
}

func (m *resourceAccessManager) failResourceAccessBestEffort(ctx context.Context, requestID string) {
	if err := m.store.FailFeishuResourceAccessRequest(requestID, m.account.ID, m.currentTime()); err != nil && !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
		feishuLog.Error(ctx, "close failed feishu resource access request=%s account=%s: %v", shortRequestID(requestID), m.account.ID, err)
	}
}

func (m *resourceAccessManager) currentTime() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *resourceAccessManager) requestTTL() time.Duration {
	if m.ttl <= 0 {
		return defaultResourceAccessTTL
	}
	return m.ttl
}

func (m *resourceAccessManager) baseContext() context.Context {
	if m.runCtx == nil {
		return context.Background()
	}
	return m.runCtx
}

func trustedResourceAccessScope(ctx context.Context) (feishutools.Actor, feishutools.ChatContext, error) {
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return feishutools.Actor{}, feishutools.ChatContext{}, fmt.Errorf("feishu resource access requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return feishutools.Actor{}, feishutools.ChatContext{}, fmt.Errorf("feishu resource access requires the trusted current chat")
	}
	return actor, chat, nil
}

func normalizeResourceAccessRequest(input feishutools.ResourceAccessRequest) (feishutools.ResourceAccessRequest, error) {
	input.ResourceType = feishutools.NormalizeResourceType(input.ResourceType)
	input.ResourceToken = strings.TrimSpace(input.ResourceToken)
	input.ResourceURL = strings.TrimSpace(input.ResourceURL)
	input.Permission = strings.ToLower(strings.TrimSpace(input.Permission))
	input.Reason = strings.TrimSpace(input.Reason)
	if !feishutools.SupportedResourceType(input.ResourceType) || input.ResourceToken == "" {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("valid feishu resource type and token are required")
	}
	if input.Permission != feishutools.ResourcePermissionRead && input.Permission != feishutools.ResourcePermissionWrite {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu resource permission must be read or write")
	}
	if input.OnceDurationMinutes < store.FeishuResourceAccessMinOnceDurationMinutes || input.OnceDurationMinutes > store.FeishuResourceAccessMaxOnceDurationMinutes {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu resource once duration must be between %d and %d minutes", store.FeishuResourceAccessMinOnceDurationMinutes, store.FeishuResourceAccessMaxOnceDurationMinutes)
	}
	if feishutools.ResourceTokenAlias(input.ResourceToken) && input.ResourceType != "folder" {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu folder aliases require resource_type=folder")
	}
	urlToken := input.ResourceToken
	if feishutools.ResourceTokenAlias(urlToken) {
		urlToken = ""
	}
	if input.ResourceURL != "" && !safeFeishuResourceURL(input.ResourceURL, urlToken) {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("resource_url must be an HTTPS Feishu/Lark resource URL containing resource_token")
	}
	return input, nil
}

func newResourceAccessOAuthValues() (state, stateHash string, err error) {
	state, err = randomBase64URL(32)
	if err != nil {
		return "", "", err
	}
	stateHash = hashResourceAccessState(state)
	return state, stateHash, nil
}

func randomBase64URL(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func hashResourceAccessState(state string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(state)))
	return hex.EncodeToString(sum[:])
}

func shortResourceRef(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func resourceAccessActorID(request store.FeishuResourceAccessRequest) string {
	if request.ActorOpenID != "" {
		return request.ActorOpenID
	}
	return request.ActorUserID
}

func resourceAccessGrantActor(openID, userID string) (string, string, error) {
	if openID = strings.TrimSpace(openID); openID != "" {
		return store.FeishuResourceGrantActorTypeOpenID, openID, nil
	}
	if userID = strings.TrimSpace(userID); userID != "" {
		return store.FeishuResourceGrantActorTypeUserID, userID, nil
	}
	return "", "", fmt.Errorf("trusted feishu resource grant actor is required")
}

func resourceAccessActorMatches(request store.FeishuResourceAccessRequest, actor feishutools.Actor) bool {
	if request.ActorOpenID != "" {
		return request.ActorOpenID == strings.TrimSpace(actor.OpenID)
	}
	return request.ActorUserID != "" && request.ActorUserID == strings.TrimSpace(actor.UserID)
}

func feishuCollaboratorPermissionSatisfies(granted, requested string, folder bool) bool {
	granted = strings.TrimSpace(granted)
	requested = strings.TrimSpace(requested)
	if requested == store.FeishuResourcePermissionRead {
		return granted == larkdrive.PermCreatePermissionMemberView || granted == larkdrive.PermCreatePermissionMemberEdit || granted == larkdrive.PermCreatePermissionMemberFullAccess
	}
	if folder {
		return granted == larkdrive.PermCreatePermissionMemberFullAccess
	}
	return granted == larkdrive.PermCreatePermissionMemberEdit || granted == larkdrive.PermCreatePermissionMemberFullAccess
}

func defaultFeishuResourceURL(resourceType, token string) string {
	token = url.PathEscape(strings.TrimSpace(token))
	switch resourceType {
	case "folder":
		return "https://docs.feishu.cn/drive/folder/" + token
	case "docx":
		return "https://docs.feishu.cn/docx/" + token
	case "doc":
		return "https://docs.feishu.cn/docs/" + token
	case "sheet":
		return "https://docs.feishu.cn/sheets/" + token
	case "bitable":
		return "https://docs.feishu.cn/base/" + token
	case "wiki":
		return "https://docs.feishu.cn/wiki/" + token
	case "file":
		return "https://docs.feishu.cn/file/" + token
	default:
		return ""
	}
}

func redirectToResource(w http.ResponseWriter, r *http.Request, rawURL string) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err == nil && safeFeishuResourceURL(target.String(), "") {
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("授权已完成，可以关闭此页面并返回飞书。"))
}

func safeFeishuResourceURL(rawURL, resourceToken string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host != "feishu.cn" && !strings.HasSuffix(host, ".feishu.cn") && host != "larksuite.com" && !strings.HasSuffix(host, ".larksuite.com") {
		return false
	}
	resourceToken = strings.TrimSpace(resourceToken)
	return resourceToken == "" || strings.Contains(parsed.EscapedPath(), url.PathEscape(resourceToken)) || strings.Contains(parsed.RawQuery, url.QueryEscape(resourceToken))
}
