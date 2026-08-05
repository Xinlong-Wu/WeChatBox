package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	defaultResourceAccessTTL               = 10 * time.Minute
	resourceAccessCallbackTimeout          = 30 * time.Second
	resourceAccessVerificationRetryTimeout = 10 * time.Second
	resourceAccessVerificationRetryDelay   = 100 * time.Millisecond
	resourceAccessDisplayNameRunes         = 120

	resourceAccessOAuthScope = "auth:user.id:read docs:permission.member:create docs:permission.member:update offline_access"

	resourceAccessOAuthStatusCapabilityReady      = "capability_ready"
	resourceAccessOAuthStatusCredentialReady      = "credential_ready"
	resourceAccessOAuthStatusAuthorizationNeeded  = "authorization_needed"
	resourceAccessOAuthStatusConfigurationMissing = "configuration_missing"
)

var (
	errFeishuResourceAccessCompletionDeferred = errors.New("feishu resource access local completion pending recovery")
	errFeishuResourceAccessOwnershipLost      = errors.New("feishu resource access runtime ownership ended; recovery required")
)

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

	credentialCipher               *feishuOAuthCredentialCipher
	refreshPeerWait                time.Duration
	refreshPoll                    time.Duration
	refreshAttemptRetention        time.Duration
	refreshAttemptCleanupInterval  time.Duration
	refreshAttemptCleanupBatchSize int
	cardUpdateTimeout              time.Duration
	discoverCapability             func(context.Context, store.FeishuResourceAccessRequest) (bool, error)
	tasks                          backgroundTaskGroup
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
		store:                          st,
		client:                         client,
		cards:                          cards,
		account:                        account,
		botOpenID:                      strings.TrimSpace(botOpenID),
		oauth:                          oauth,
		runCtx:                         runCtx,
		ttl:                            defaultResourceAccessTTL,
		now:                            time.Now,
		credentialCipher:               credentialCipher,
		refreshPeerWait:                feishuOAuthRefreshPeerWait,
		refreshPoll:                    feishuOAuthRefreshPollInterval,
		refreshAttemptRetention:        defaultFeishuOAuthRefreshAttemptRetention,
		refreshAttemptCleanupInterval:  defaultFeishuOAuthRefreshAttemptCleanupInterval,
		refreshAttemptCleanupBatchSize: defaultFeishuOAuthRefreshAttemptCleanupBatchSize,
		cardUpdateTimeout:              defaultFeishuCardUpdateTimeout,
	}
	if err := cards.RegisterAction(resourceAccessCardActionKind, manager.HandleCardAction); err != nil {
		return nil, fmt.Errorf("register feishu resource access card action: %w", err)
	}
	return manager, nil
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
	resourceDisplayName, err := m.resolveResourceDisplayName(chat, resourceType, resourceToken)
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
		ResourceDisplayName: resourceDisplayName,
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
	if !capabilityActive {
		capabilityActive, err = m.discoverTenantCapability(ctx, request)
		if err != nil {
			m.failResourceAccessBestEffort(ctx, request.ID)
			return feishutools.ResourceAccessResult{}, fmt.Errorf("discover existing feishu resource capability: %w", err)
		}
	}
	if !capabilityActive && !m.oauthEnabled() {
		m.failResourceAccessBestEffort(ctx, request.ID)
		result.Status = feishutools.ResourceAccessStatusUnsupported
		result.Message = "当前机器人账号未配置 OAuth，且没有可复用的飞书资源能力；只能直接使用 Bot 自有资源或已经可以实时验证的授权。"
		return result, nil
	}
	oauthStatus, err := m.resourceAccessOAuthDisplayStatus(actor, capabilityActive)
	if err != nil {
		m.failResourceAccessBestEffort(ctx, request.ID)
		return feishutools.ResourceAccessResult{}, fmt.Errorf("inspect feishu resource OAuth availability: %w", err)
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
	messageID, err := m.cards.Send(ctx, request.ChatID, pendingResourceGrantCard{request: request, oauthStatus: oauthStatus})
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

func (m *resourceAccessManager) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	requestID, action, ok := parseResourceAccessCardAction(event)
	if !ok {
		return nil, nil
	}
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Context == nil {
		return cardToast("error", "授权回调信息不完整，请重新发起。"), nil
	}
	if m.baseContext().Err() != nil {
		message := "LingoBridge 正在关闭，请服务恢复后重新点击本卡片。"
		if action == resourceAccessCardActionSubmitOAuth {
			message = "LingoBridge 正在关闭，请服务恢复后重新提交本卡片中的授权结果。"
		}
		feishuLog.Warn(ctx, "rejected feishu resource card callback after runtime cancellation request=%s account=%s action=%s",
			shortRequestID(requestID), m.account.ID, action)
		return cardToast("error", message), nil
	}
	operator := event.Event.Operator
	match := store.FeishuResourceAccessMatch{
		ActorOpenID:   operator.OpenID,
		ActorUserID:   deref(operator.UserID),
		ChatID:        event.Event.Context.OpenChatID,
		CardMessageID: event.Event.Context.OpenMessageID,
	}
	releaseTask, accepted := m.tasks.Reserve()
	if !accepted {
		message := "LingoBridge 正在关闭，请服务恢复后重新点击本卡片。"
		if action == resourceAccessCardActionSubmitOAuth {
			message = "LingoBridge 正在关闭，请服务恢复后重新提交本卡片中的授权结果。"
		}
		feishuLog.Warn(ctx, "rejected feishu resource card callback while runtime is shutting down request=%s account=%s action=%s",
			shortRequestID(requestID), m.account.ID, action)
		return cardToast("error", message), nil
	}
	taskHandedOff := false
	defer func() {
		if !taskHandedOff {
			releaseTask()
		}
	}()
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
		taskHandedOff = true
		go func() {
			defer releaseTask()
			m.completeApprovedResourceAccessFromCard(request)
		}()
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
		taskHandedOff = true
		go func() {
			defer releaseTask()
			m.completeResourceAccessOAuthFromCard(request, submission.Response)
		}()
		return cardToast("success", "已收到授权结果，正在核验并授予资源权限；完成后机器人会更新卡片并通知当前对话。"), nil
	default:
		return cardToast("error", "不支持的资源授权操作。"), nil
	}
}
