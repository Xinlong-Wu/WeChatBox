package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken"
	"github.com/larksuite/oapi-sdk-go/v3/core/accesstoken/authorizationcode"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

func (m *resourceAccessManager) prepareResourceAccessOAuthHandoff(ctx context.Context, request store.FeishuResourceAccessRequest) error {
	if !m.oauthEnabled() {
		return fmt.Errorf("feishu OAuth callback is not configured")
	}
	state, stateHash, err := newResourceAccessOAuthValues()
	if err != nil {
		return fmt.Errorf("generate feishu resource OAuth state: %w", err)
	}
	stateCiphertext, err := m.encryptResourceAccessOAuthState(request, state)
	if err != nil {
		return fmt.Errorf("encrypt feishu resource OAuth state: %w", err)
	}
	if err := m.store.PrepareFeishuResourceAccessOAuth(
		request.ID,
		request.AccountID,
		stateHash,
		stateCiphertext,
		"",
		request.SubjectType,
		request.SubjectID,
		m.currentTime(),
	); err != nil {
		if errors.Is(err, store.ErrFeishuResourceAccessResolved) {
			recoveredExisting := false
			current, loadErr := m.store.GetFeishuResourceAccessRequest(request.ID, request.AccountID)
			if loadErr == nil && current.State == store.FeishuResourceAccessStatePending &&
				current.OAuthStateHash != "" && current.GrantMode == request.GrantMode &&
				current.SubjectType == request.SubjectType && current.SubjectID == request.SubjectID &&
				m.currentTime().Before(current.ExpiresAt) {
				if !current.OAuthHandoffDeliveredAt.IsZero() {
					feishuLog.Debug(ctx, "kept delivered feishu resource OAuth handoff prepared by another worker request=%s account=%s state_ref=%s",
						shortRequestID(request.ID), request.AccountID, shortResourceRef(current.OAuthStateHash))
					return nil
				}
				if current.OAuthStateCiphertext == "" {
					feishuLog.Debug(ctx, "kept legacy feishu resource OAuth handoff without recoverable delivery state request=%s account=%s state_ref=%s",
						shortRequestID(request.ID), request.AccountID, shortResourceRef(current.OAuthStateHash))
					return nil
				}
				state, loadErr = m.decryptResourceAccessOAuthState(current)
				if loadErr != nil {
					return fmt.Errorf("recover feishu resource OAuth state: %w", loadErr)
				}
				if hashResourceAccessState(state) != current.OAuthStateHash {
					return fmt.Errorf("recover feishu resource OAuth state: authenticated state hash mismatch")
				}
				request = current
				stateHash = current.OAuthStateHash
				recoveredExisting = true
			}
			if loadErr != nil {
				return fmt.Errorf("reload prepared feishu resource OAuth request: %w", loadErr)
			}
			if !recoveredExisting {
				return fmt.Errorf("prepare feishu resource OAuth request: %w", err)
			}
		} else {
			return fmt.Errorf("prepare feishu resource OAuth request: %w", err)
		}
	} else {
		request.OAuthStateHash = stateHash
		request.OAuthStateCiphertext = stateCiphertext
	}
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
	if err := m.updateResourceAccessOAuthHandoffCard(request, pendingResourceAccessCard{request: request, authURL: authURL}); err != nil {
		feishuLog.Warn(ctx, "deferred feishu resource OAuth handoff card to durable delivery worker request=%s account=%s card_message=%s error_type=%T",
			shortRequestID(request.ID), request.AccountID, request.CardMessageID, err)
		return nil
	}
	deliveredAt := m.currentTime()
	if err := m.store.MarkFeishuResourceAccessOAuthHandoffDelivered(request.ID, request.AccountID, stateHash, deliveredAt); err != nil {
		if !errors.Is(err, store.ErrFeishuResourceAccessResolved) {
			feishuLog.Warn(ctx, "record feishu resource OAuth card delivery failed; recovery may resend the same card request=%s account=%s state_ref=%s: %v",
				shortRequestID(request.ID), request.AccountID, shortResourceRef(stateHash), err)
		}
	} else {
		request.OAuthHandoffDeliveredAt = deliveredAt
	}
	feishuLog.Info(ctx, "requested feishu resource OAuth handoff request=%s account=%s user=%s chat=%s card_message=%s type=%s resource_ref=%s permission=%s grant_mode=%s state_ref=%s expires_at=%s",
		shortRequestID(request.ID), request.AccountID, resourceAccessActorID(request), request.ChatID,
		request.CardMessageID, request.ResourceType, shortResourceRef(request.ResourceToken), request.Permission,
		request.GrantMode, shortResourceRef(stateHash), request.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (m *resourceAccessManager) updateResourceAccessOAuthHandoffCard(request store.FeishuResourceAccessRequest, card Card) error {
	return m.updateResourceCardWithFreshTimeout(request.CardMessageID, card)
}

func (m *resourceAccessManager) encryptResourceAccessOAuthState(request store.FeishuResourceAccessRequest, state string) (string, error) {
	if m == nil || m.credentialCipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	return m.credentialCipher.Encrypt(
		feishuOAuthIdentity{OpenID: request.ActorOpenID, UserID: request.ActorUserID},
		resourceAccessOAuthStateCipherField(request.ID),
		state,
	)
}

func (m *resourceAccessManager) decryptResourceAccessOAuthState(request store.FeishuResourceAccessRequest) (string, error) {
	if m == nil || m.credentialCipher == nil {
		return "", ErrFeishuUserOAuthCredentialUnavailable
	}
	return m.credentialCipher.Decrypt(
		feishuOAuthIdentity{OpenID: request.ActorOpenID, UserID: request.ActorUserID},
		resourceAccessOAuthStateCipherField(request.ID),
		request.OAuthStateCiphertext,
	)
}

func resourceAccessOAuthStateCipherField(requestID string) string {
	return "resource_access_oauth_state:" + strings.TrimSpace(requestID)
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
	// Once the card callback atomically consumes an authorization code, runtime
	// cancellation must not strand that one-shot value. The task-group lease
	// keeps dependencies alive while this bounded completion drains.
	drainCtx, cancelDrain := feishuRuntimeDrainContext(m.baseContext())
	defer cancelDrain()
	ctx, cancel := context.WithTimeout(drainCtx, resourceAccessCallbackTimeout)
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
	if m.baseContext().Err() != nil {
		feishuLog.Warn(r.Context(), "rejected feishu resource OAuth HTTP callback after runtime cancellation account=%s", m.account.ID)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "LingoBridge is shutting down; retry the callback after restart", http.StatusServiceUnavailable)
		return
	}
	releaseTask, accepted := m.tasks.Reserve()
	if !accepted {
		feishuLog.Warn(r.Context(), "rejected feishu resource OAuth HTTP callback while runtime is shutting down account=%s", m.account.ID)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "LingoBridge is shutting down; retry the callback after restart", http.StatusServiceUnavailable)
		return
	}
	defer releaseTask()
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
			expiredAt := m.currentTime()
			request.UpdatedAt = expiredAt
			message := "资源授权请求已过期，请重新调用资源授权工具。"
			m.updateResourceAccessResultCard(r.Context(), request, statusCard{title: "授权已过期", template: "grey", message: message})
			m.persistResourceWorkflowResult(r.Context(), request, store.WorkflowResultStateExpired, "expired", "", message, expiredAt)
		}
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrFeishuResourceAccessExpired) {
			status = http.StatusGone
		}
		feishuLog.Warn(r.Context(), "reject feishu resource OAuth callback account=%s state_ref=%s: %v", m.account.ID, shortResourceRef(hashResourceAccessState(query.State)), err)
		http.Error(w, "OAuth request is invalid, expired, or already used", status)
		return
	}
	// Browser disconnects and runtime cancellation must not interrupt a code
	// after its state has been claimed. The reservation above keeps the manager
	// alive until this bounded completion returns.
	drainCtx, cancelDrain := feishuRuntimeDrainContext(m.baseContext())
	defer cancelDrain()
	callbackCtx, cancel := context.WithTimeout(drainCtx, resourceAccessCallbackTimeout)
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
