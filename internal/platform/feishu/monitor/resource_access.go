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
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken/authorizationcode"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	defaultResourceAccessTTL      = 10 * time.Minute
	resourceAccessCallbackTimeout = 30 * time.Second
	resourceAccessNotifyTimeout   = 10 * time.Second

	resourceAccessOAuthScope = "auth:user.id:read docs:permission.member:create"
)

type resourceAccessStore interface {
	PlatformID() string
	SaveFeishuBotResource(store.FeishuBotResource) (store.FeishuBotResource, error)
	GetFeishuBotResource(accountID, resourceType, resourceToken string) (store.FeishuBotResource, error)
	DefaultFeishuChatFolder(accountID, chatID string) (store.FeishuChatFolder, error)
	CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest) (store.FeishuResourceAccessRequest, error)
	PrepareFeishuResourceAccessOAuth(id, accountID, stateHash, verifier, subjectType, subjectID string, now time.Time) error
	SetFeishuResourceAccessCardMessageID(id, accountID, messageID string, now time.Time) error
	GetFeishuResourceAccessRequest(id, accountID string) (store.FeishuResourceAccessRequest, error)
	ConsumeFeishuResourceAccessRequest(id, accountID, consumedByRequestID string, now time.Time) error
	ClaimFeishuResourceAccessOAuth(stateHash, accountID string, now time.Time) (store.FeishuResourceAccessRequest, error)
	DenyFeishuResourceAccessRequest(id, accountID string, match store.FeishuResourceAccessMatch, now time.Time) (store.FeishuResourceAccessRequest, error)
	CompleteFeishuResourceAccessRequest(id, accountID, source, verifiedPermission string, grant *store.FeishuResourceGrant, now time.Time) error
	FailFeishuResourceAccessRequest(id, accountID string, now time.Time) error
	ExpireFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error)
	FailExecutingFeishuResourceAccessRequests(accountID string, now time.Time) (int64, error)
	UpsertFeishuResourceGrant(store.FeishuResourceGrant) (store.FeishuResourceGrant, error)
	ActiveFeishuResourceGrant(accountID, chatID, resourceType, resourceToken, permission string) (store.FeishuResourceGrant, bool, error)
	RevokeFeishuResourceGrant(accountID, chatID, resourceType, resourceToken string, now time.Time) error
}

type resourceAccessOAuthConfig struct {
	ClientID              string
	BaseURL               string
	CallbackURL           string
	CallbackListenAddress string
}

type resourceAccessNotifier interface {
	SendText(ctx context.Context, chatID, text string) error
}

type resourceAccessManager struct {
	store     resourceAccessStore
	client    *lark.Client
	cards     CardService
	notifier  resourceAccessNotifier
	account   store.Account
	botOpenID string
	oauth     resourceAccessOAuthConfig
	runCtx    context.Context
	ttl       time.Duration
	now       func() time.Time
}

var _ feishutools.ResourceAccessController = (*resourceAccessManager)(nil)

func newResourceAccessManager(
	runCtx context.Context,
	st *store.Store,
	client *lark.Client,
	account store.Account,
	botOpenID string,
	cards CardService,
	notifier resourceAccessNotifier,
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
	if cards == nil || notifier == nil {
		return nil, fmt.Errorf("feishu resource access cards and notifier are required")
	}
	oauth.ClientID = strings.TrimSpace(oauth.ClientID)
	oauth.BaseURL = strings.TrimRight(strings.TrimSpace(oauth.BaseURL), "/")
	oauth.CallbackURL = strings.TrimSpace(oauth.CallbackURL)
	oauth.CallbackListenAddress = strings.TrimSpace(oauth.CallbackListenAddress)
	if (oauth.CallbackURL == "") != (oauth.CallbackListenAddress == "") {
		return nil, fmt.Errorf("feishu oauth_callback_url and oauth_callback_listen_address must be configured together")
	}
	if oauth.CallbackURL != "" {
		parsed, err := url.Parse(oauth.CallbackURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("feishu oauth_callback_url must be an absolute URL")
		}
		if oauth.ClientID == "" || oauth.BaseURL == "" {
			return nil, fmt.Errorf("feishu OAuth client_id and oauth_base_url are required when the callback is enabled")
		}
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	manager := &resourceAccessManager{
		store:     st,
		client:    client,
		cards:     cards,
		notifier:  notifier,
		account:   account,
		botOpenID: strings.TrimSpace(botOpenID),
		oauth:     oauth,
		runCtx:    runCtx,
		ttl:       defaultResourceAccessTTL,
		now:       time.Now,
	}
	if err := cards.RegisterAction(resourceAccessCardActionKind, manager.HandleCardAction); err != nil {
		return nil, fmt.Errorf("register feishu resource access card action: %w", err)
	}
	return manager, nil
}

func (m *resourceAccessManager) recoverPersistedRequests(ctx context.Context) error {
	now := m.currentTime()
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
	if interrupted > 0 {
		feishuLog.Warn(ctx, "closed interrupted feishu resource access requests account=%s count=%d", m.account.ID, interrupted)
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
	now := m.currentTime()
	request, err := m.store.CreateFeishuResourceAccessRequest(store.FeishuResourceAccessRequest{
		AccountID:       m.account.ID,
		ActorOpenID:     actor.OpenID,
		ActorUserID:     actor.UserID,
		ChatID:          chat.ChatID,
		SourceMessageID: chat.MessageID,
		ResourceType:    resourceType,
		ResourceToken:   resourceToken,
		ResourceURL:     resourceURL,
		Permission:      input.Permission,
		Reason:          input.Reason,
		CreatedAt:       now,
		ExpiresAt:       now.Add(m.requestTTL()),
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

	grant, active, err := m.store.ActiveFeishuResourceGrant(request.AccountID, request.ChatID, resourceType, resourceToken, request.Permission)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("check feishu resource grant: %w", err)
	}
	if active {
		verified, verifyErr := m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, request.Permission, grant.SubjectType, grant.SubjectID)
		if verifyErr != nil {
			feishuLog.Warn(ctx, "live-check existing feishu resource grant failed request=%s account=%s chat=%s type=%s resource_ref=%s: %v",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType, shortResourceRef(request.ResourceToken), verifyErr)
		}
		if verified {
			grant.SourceRequestID = request.ID
			grant.VerifiedAt = m.currentTime()
			grant.UpdatedAt = grant.VerifiedAt
			if err := m.store.CompleteFeishuResourceAccessRequest(
				request.ID,
				request.AccountID,
				store.FeishuResourceGrantSourceExistingGrant,
				grant.Permission,
				&grant,
				grant.VerifiedAt,
			); err != nil {
				return feishutools.ResourceAccessResult{}, fmt.Errorf("complete existing feishu resource grant: %w", err)
			}
			feishuLog.Info(ctx, "reused verified feishu resource grant request=%s account=%s chat=%s type=%s resource_ref=%s permission=%s",
				shortRequestID(request.ID), request.AccountID, request.ChatID, request.ResourceType, shortResourceRef(request.ResourceToken), grant.Permission)
			return m.resourceAccessResult(request, feishutools.ResourceAccessStatusGranted, store.FeishuResourceGrantSourceExistingGrant), nil
		}
		if err := m.store.RevokeFeishuResourceGrant(request.AccountID, request.ChatID, request.ResourceType, request.ResourceToken, m.currentTime()); err != nil && !errors.Is(err, store.ErrFeishuResourceGrantNotFound) {
			feishuLog.Warn(ctx, "revoke stale feishu resource grant failed request=%s account=%s chat=%s: %v", shortRequestID(request.ID), request.AccountID, request.ChatID, err)
		}
	}

	subjectType, subjectID, supportedMessage := m.resourceGrantSubject(chat, request.ResourceType)
	if subjectType == "" {
		m.failResourceAccessBestEffort(ctx, request.ID)
		result.Status = feishutools.ResourceAccessStatusUnsupported
		result.Message = supportedMessage
		return result, nil
	}
	if !m.oauthEnabled() {
		m.failResourceAccessBestEffort(ctx, request.ID)
		result.Status = feishutools.ResourceAccessStatusUnsupported
		result.Message = "当前机器人账号未配置 OAuth 回调；只能直接使用 Bot 自有资源或已经可以实时验证的授权。"
		return result, nil
	}
	state, stateHash, verifier, challenge, err := newResourceAccessOAuthValues()
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("generate feishu resource OAuth state: %w", err)
	}
	if err := m.store.PrepareFeishuResourceAccessOAuth(
		request.ID,
		request.AccountID,
		stateHash,
		verifier,
		subjectType,
		subjectID,
		m.currentTime(),
	); err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("prepare feishu resource OAuth request: %w", err)
	}
	request.SubjectType = subjectType
	request.SubjectID = subjectID
	authURL, err := m.authorizationURL(state, challenge)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, err
	}
	messageID, err := m.cards.Send(ctx, request.ChatID, pendingResourceAccessCard{request: request, authURL: authURL})
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("send feishu resource access card: %w", err)
	}
	if err := m.store.SetFeishuResourceAccessCardMessageID(request.ID, request.AccountID, messageID, m.currentTime()); err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		m.updateResourceCardBestEffort(ctx, messageID, statusCard{title: "授权请求失败", template: "red", message: "授权请求未能保存，请重新发起。"})
		return feishutools.ResourceAccessResult{}, fmt.Errorf("bind feishu resource access card: %w", err)
	}
	request.CardMessageID = messageID
	feishuLog.Info(ctx, "requested feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s expires_at=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission, request.ExpiresAt.Format(time.RFC3339))
	result.ExpiresAt = request.ExpiresAt
	result.Message = "已向当前飞书用户发送资源授权卡片。请在卡片中打开飞书官方授权页；完成后机器人会更新卡片并通知当前对话。"
	return result, nil
}

func (m *resourceAccessManager) ValidateAccess(ctx context.Context, validation feishutools.ResourceAccessValidation) (feishutools.ResourceAccessResult, error) {
	actor, chat, err := trustedResourceAccessScope(ctx)
	if err != nil {
		return feishutools.ResourceAccessResult{}, err
	}
	validation.RequestID = strings.TrimSpace(validation.RequestID)
	validation.ResourceType = feishutools.NormalizeResourceType(validation.ResourceType)
	validation.ResourceToken = strings.TrimSpace(validation.ResourceToken)
	validation.Permission = strings.ToLower(strings.TrimSpace(validation.Permission))
	if validation.RequestID == "" || !feishutools.SupportedResourceType(validation.ResourceType) || validation.ResourceToken == "" ||
		(validation.Permission != feishutools.ResourcePermissionRead && validation.Permission != feishutools.ResourcePermissionWrite) {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("valid access_request_id, resource type/token, and permission are required")
	}
	request, err := m.store.GetFeishuResourceAccessRequest(validation.RequestID, m.account.ID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuResourceAccessNotFound) {
			return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id is unknown for the current Bot account")
		}
		return feishutools.ResourceAccessResult{}, fmt.Errorf("load feishu resource access request: %w", err)
	}
	if request.State != store.FeishuResourceAccessStateSucceeded {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id is not granted; current state is %s", request.State)
	}
	if request.ChatID != chat.ChatID || !resourceAccessActorMatches(request, actor) {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id does not belong to the current Feishu user and chat")
	}
	if request.ResourceType != validation.ResourceType || request.ResourceToken != validation.ResourceToken {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id does not match the requested resource")
	}
	now := m.currentTime()
	if !request.ExpiresAt.After(now) {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id has expired; call %s again", feishutools.ResourceAccessToolName)
	}
	if request.ConsumedByRequestID != "" {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id has already been used; call %s again", feishutools.ResourceAccessToolName)
	}
	if !store.FeishuResourcePermissionSatisfies(request.VerifiedPermission, validation.Permission) {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id does not grant the required %s permission", validation.Permission)
	}
	if request.GrantSource == store.FeishuResourceGrantSourceBotOwner {
		if _, err := m.store.GetFeishuBotResource(request.AccountID, request.ResourceType, request.ResourceToken); err != nil {
			return feishutools.ResourceAccessResult{}, fmt.Errorf("Bot-owned resource record is no longer available; call %s again", feishutools.ResourceAccessToolName)
		}
		return m.resourceAccessResult(request, feishutools.ResourceAccessStatusGranted, request.GrantSource), nil
	}
	verified, verifyErr := m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, validation.Permission, request.SubjectType, request.SubjectID)
	if verifyErr != nil {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("live-check feishu resource access: %w", verifyErr)
	}
	if !verified {
		if err := m.store.RevokeFeishuResourceGrant(request.AccountID, request.ChatID, request.ResourceType, request.ResourceToken, m.currentTime()); err != nil && !errors.Is(err, store.ErrFeishuResourceGrantNotFound) {
			feishuLog.Warn(ctx, "revoke invalid create-time feishu resource grant failed request=%s account=%s chat=%s: %v",
				shortRequestID(request.ID), request.AccountID, request.ChatID, err)
		}
		return feishutools.ResourceAccessResult{}, fmt.Errorf("Feishu no longer reports the required access; call %s again", feishutools.ResourceAccessToolName)
	}
	if _, err := m.store.UpsertFeishuResourceGrant(store.FeishuResourceGrant{
		AccountID:       request.AccountID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      request.VerifiedPermission,
		SubjectType:     request.SubjectType,
		SubjectID:       request.SubjectID,
		SourceRequestID: request.ID,
		State:           store.FeishuResourceGrantStateActive,
		CreatedAt:       request.CreatedAt,
		VerifiedAt:      m.currentTime(),
	}); err != nil {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("refresh feishu resource grant verification: %w", err)
	}
	return m.resourceAccessResult(request, feishutools.ResourceAccessStatusGranted, request.GrantSource), nil
}

func (m *resourceAccessManager) ConsumeAccess(ctx context.Context, validation feishutools.ResourceAccessValidation, consumingRequestID string) (feishutools.ResourceAccessResult, error) {
	consumingRequestID = strings.TrimSpace(consumingRequestID)
	if consumingRequestID == "" {
		return feishutools.ResourceAccessResult{}, fmt.Errorf("consuming workflow request_id is required")
	}
	result, err := m.ValidateAccess(ctx, validation)
	if err != nil {
		return feishutools.ResourceAccessResult{}, err
	}
	if err := m.store.ConsumeFeishuResourceAccessRequest(result.RequestID, m.account.ID, consumingRequestID, m.currentTime()); err != nil {
		switch {
		case errors.Is(err, store.ErrFeishuResourceAccessExpired):
			return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id has expired; call %s again", feishutools.ResourceAccessToolName)
		case errors.Is(err, store.ErrFeishuResourceAccessConsumed):
			return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id has already been used; call %s again", feishutools.ResourceAccessToolName)
		case errors.Is(err, store.ErrFeishuResourceAccessResolved):
			return feishutools.ResourceAccessResult{}, fmt.Errorf("access_request_id is no longer granted; call %s again", feishutools.ResourceAccessToolName)
		default:
			return feishutools.ResourceAccessResult{}, fmt.Errorf("consume feishu resource access request: %w", err)
		}
	}
	feishuLog.Debug(ctx, "consumed feishu resource access request=%s operation=%s account=%s type=%s resource_ref=%s permission=%s",
		shortRequestID(result.RequestID), shortRequestID(consumingRequestID), m.account.ID,
		result.ResourceType, shortResourceRef(result.ResourceToken), result.Permission)
	return result, nil
}

func (m *resourceAccessManager) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	requestID, action, ok := parseResourceAccessCardAction(event)
	if !ok || action != resourceAccessCardActionReject {
		return nil, nil
	}
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Context == nil {
		return cardToast("error", "授权回调信息不完整，请重新发起。"), nil
	}
	operator := event.Event.Operator
	request, err := m.store.DenyFeishuResourceAccessRequest(requestID, m.account.ID, store.FeishuResourceAccessMatch{
		ActorOpenID:   operator.OpenID,
		ActorUserID:   deref(operator.UserID),
		ChatID:        event.Event.Context.OpenChatID,
		CardMessageID: event.Event.Context.OpenMessageID,
	}, m.currentTime())
	if err != nil {
		return m.resourceAccessDecisionError(ctx, request, requestID, err), nil
	}
	feishuLog.Info(ctx, "denied feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken))
	return approvalCallbackResponse(
		"success",
		"已拒绝，本次不会授予资源权限。",
		statusCard{title: "已拒绝授权", template: "grey", message: "请求已取消，未授予资源权限。"},
	), nil
}

type resourceAccessOAuthResponse struct {
	Code  string
	Error string
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

func (m *resourceAccessManager) HandleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		http.Error(w, "missing OAuth state", http.StatusBadRequest)
		return
	}
	request, err := m.store.ClaimFeishuResourceAccessOAuth(hashResourceAccessState(state), m.account.ID, m.currentTime())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrFeishuResourceAccessExpired) {
			status = http.StatusGone
		}
		feishuLog.Warn(r.Context(), "reject feishu resource OAuth callback account=%s state_ref=%s: %v", m.account.ID, shortResourceRef(hashResourceAccessState(state)), err)
		http.Error(w, "OAuth request is invalid, expired, or already used", status)
		return
	}
	callbackCtx, cancel := context.WithTimeout(m.baseContext(), resourceAccessCallbackTimeout)
	defer cancel()
	err = m.completeResourceAccessOAuth(callbackCtx, request, resourceAccessOAuthResponse{
		Code:  r.URL.Query().Get("code"),
		Error: r.URL.Query().Get("error"),
	})
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
	accessToken, err := m.exchangeAuthorizationCode(ctx, code, request.PKCEVerifier)
	if err != nil {
		return fail(err, "授权失败", "飞书授权码兑换失败，请重新发起。", http.StatusBadGateway, "authorization code exchange failed")
	}
	if err := m.verifyOAuthUser(ctx, accessToken, request); err != nil {
		return fail(err, "授权用户不匹配", "只有发起资源请求的飞书用户可以完成授权。", http.StatusForbidden, "authorized user does not match the requester")
	}
	createCode, err := m.grantResourceAccess(ctx, accessToken, request)
	if err != nil && createCode != 1063003 {
		return fail(err, "授予权限失败", "飞书未能把所需权限授予机器人或当前群聊，请重新检查资源权限。", http.StatusBadGateway, "granting resource access failed")
	}
	verified, verifyErr := m.verifyTenantAccess(ctx, request.ResourceType, request.ResourceToken, request.Permission, request.SubjectType, request.SubjectID)
	if verifyErr != nil || !verified {
		if verifyErr == nil {
			verifyErr = fmt.Errorf("permission verification returned false")
		}
		return fail(verifyErr, "权限核验失败", "飞书没有确认机器人或当前群聊已获得所需权限。", http.StatusBadGateway, "resource access verification failed")
	}
	completedAt := m.currentTime()
	grant := store.FeishuResourceGrant{
		AccountID:       request.AccountID,
		ChatID:          request.ChatID,
		ResourceType:    request.ResourceType,
		ResourceToken:   request.ResourceToken,
		Permission:      request.Permission,
		SubjectType:     request.SubjectType,
		SubjectID:       request.SubjectID,
		SourceRequestID: request.ID,
		State:           store.FeishuResourceGrantStateActive,
		CreatedAt:       completedAt,
		VerifiedAt:      completedAt,
	}
	if err := m.store.CompleteFeishuResourceAccessRequest(
		request.ID,
		request.AccountID,
		store.FeishuResourceGrantSourceNewlyGranted,
		request.Permission,
		&grant,
		completedAt,
	); err != nil {
		return fail(err, "授权记录失败", "飞书已授予权限，但 LingoBridge 未能保存核验结果；请重新调用授权工具核验。", http.StatusInternalServerError, "saving resource access failed")
	}
	feishuLog.Info(ctx, "granted feishu resource access request=%s account=%s user=%s chat=%s type=%s resource_ref=%s permission=%s subject_type=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission, request.SubjectType)
	m.notifyResourceAccessResult(ctx, request,
		statusCard{title: "权限已授予", template: "green", message: "飞书已确认所需资源权限。现在可以使用该 request_id 继续原操作。"},
		fmt.Sprintf("✅ 飞书资源权限已授予。request_id：`%s`，可继续原操作。", request.ID),
	)
	return nil
}

func (m *resourceAccessManager) exchangeAuthorizationCode(ctx context.Context, code, verifier string) (string, error) {
	resp, err := m.client.AccessToken.RetrieveByAuthorizationCode(ctx, authorizationcode.NewTokenRequestBuilder().
		Code(code).
		RedirectUri(m.oauth.CallbackURL).
		CodeVerifier(verifier).
		Build())
	if err != nil {
		return "", fmt.Errorf("exchange feishu OAuth authorization code: %w", err)
	}
	if resp == nil || !resp.Success() || resp.Data == nil {
		return "", fmt.Errorf("exchange feishu OAuth authorization code: empty or unsuccessful response")
	}
	token := strings.TrimSpace(deref(resp.Data.AccessToken))
	if token == "" {
		return "", fmt.Errorf("exchange feishu OAuth authorization code returned no access_token")
	}
	return token, nil
}

func (m *resourceAccessManager) verifyOAuthUser(ctx context.Context, accessToken string, request store.FeishuResourceAccessRequest) error {
	resp, err := m.client.Authen.UserInfo.Get(ctx, larkcore.WithUserAccessToken(accessToken))
	if err != nil {
		return fmt.Errorf("get authorized feishu user: %w", err)
	}
	if resp == nil || !resp.Success() || resp.Data == nil {
		if resp == nil {
			return fmt.Errorf("get authorized feishu user: empty response")
		}
		return fmt.Errorf("get authorized feishu user code=%d msg=%s", resp.Code, resp.Msg)
	}
	openID := strings.TrimSpace(deref(resp.Data.OpenId))
	userID := strings.TrimSpace(deref(resp.Data.UserId))
	if request.ActorOpenID != "" {
		if request.ActorOpenID != openID {
			return fmt.Errorf("authorized open_id does not match requester")
		}
		return nil
	}
	if request.ActorUserID == "" || request.ActorUserID != userID {
		return fmt.Errorf("authorized user_id does not match requester")
	}
	return nil
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

func (m *resourceAccessManager) authorizationURL(state, challenge string) (string, error) {
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
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func (m *resourceAccessManager) oauthEnabled() bool {
	return m.oauth.ClientID != "" && m.oauth.BaseURL != "" && m.oauth.CallbackURL != "" && m.oauth.CallbackListenAddress != ""
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
		return cardToast("error", "只有发起请求的用户可以拒绝该授权。")
	case errors.Is(err, store.ErrFeishuResourceAccessContextMismatch):
		return cardToast("error", "授权卡片与原请求不匹配。")
	case errors.Is(err, store.ErrFeishuResourceAccessExpired):
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

func (m *resourceAccessManager) finishOAuthFailure(ctx context.Context, request store.FeishuResourceAccessRequest, cause error, title, message string) {
	if err := m.store.FailFeishuResourceAccessRequest(request.ID, request.AccountID, m.currentTime()); err != nil && !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
		feishuLog.Error(ctx, "mark feishu resource OAuth failed request=%s account=%s: %v", shortRequestID(request.ID), request.AccountID, err)
	}
	feishuLog.Warn(ctx, "feishu resource OAuth failed request=%s account=%s user=%s chat=%s type=%s resource_ref=%s: %v",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.ResourceType, shortResourceRef(request.ResourceToken), cause)
	m.notifyResourceAccessResult(ctx, request, statusCard{title: title, template: "red", message: message}, "❌ "+message)
}

func (m *resourceAccessManager) notifyResourceAccessResult(ctx context.Context, request store.FeishuResourceAccessRequest, card Card, message string) {
	notifyCtx, cancel := context.WithTimeout(ctx, resourceAccessNotifyTimeout)
	defer cancel()
	m.updateResourceCardBestEffort(notifyCtx, request.CardMessageID, card)
	if err := m.notifier.SendText(notifyCtx, request.ChatID, message); err != nil {
		feishuLog.Warn(notifyCtx, "send feishu resource access result failed request=%s account=%s chat=%s: %v", shortRequestID(request.ID), request.AccountID, request.ChatID, err)
	}
}

func (m *resourceAccessManager) updateResourceCardBestEffort(ctx context.Context, messageID string, card Card) {
	if strings.TrimSpace(messageID) == "" || card == nil {
		return
	}
	if err := m.cards.UpdateByMessageID(ctx, messageID, card); err != nil {
		feishuLog.Warn(ctx, "update feishu resource access card failed message=%s: %v", messageID, err)
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

func newResourceAccessOAuthValues() (state, stateHash, verifier, challenge string, err error) {
	state, err = randomBase64URL(32)
	if err != nil {
		return "", "", "", "", err
	}
	verifier, err = randomBase64URL(48)
	if err != nil {
		return "", "", "", "", err
	}
	stateHash = hashResourceAccessState(state)
	challengeSum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(challengeSum[:])
	return state, stateHash, verifier, challenge, nil
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
