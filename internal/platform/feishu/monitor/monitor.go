package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"lingobridge/internal/config"
	"lingobridge/internal/core"
	"lingobridge/internal/logging"
	"lingobridge/internal/platform/feishu"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/session"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

var (
	feishuLog    = logging.For("feishu")
	feishuSDKLog = logging.For("feishu/lark")
)

type starter interface {
	Start(ctx context.Context) error
}

type closer interface {
	Close()
}

func RunContext(ctx context.Context, st *store.Store, sm *session.Manager, cfg config.LLMConfig, acc store.Account) error {
	return fmt.Errorf("feishu RunContext requires resolved platform account config")
}

type Platform struct {
	store   *store.Store
	account store.Account
	config  feishu.Config
	level   logging.Level
}

var _ core.Platform = (*Platform)(nil)

func NewPlatform(st *store.Store, acc store.Account, cfg feishu.Config, level logging.Level) *Platform {
	cfg.ApplyDefaults()
	return &Platform{store: st, account: acc, config: cfg, level: level}
}

func (p *Platform) Run(ctx context.Context, handler core.Handler) (runErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ownershipCtx, cancelOwnership := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelOwnership()
	lifecycleCtx, cancelLifecycle := context.WithCancel(withFeishuRuntimeOwnership(ctx, ownershipCtx))
	var (
		approvals         *operationApprovalService
		resourceAccess    *resourceAccessManager
		continuationDone  <-chan struct{}
		cardDeliveryDone  <-chan struct{}
		messageBot        *bot
		runtimeLease      *feishuAccountRuntimeLease
		leaseHeartbeat    *feishuAccountRuntimeHeartbeat
		leaseHeartbeatErr error
	)
	defer func() {
		var releaseErr error
		leaseHeartbeatErr, releaseErr = shutdownFeishuAccountRuntime(
			cancelLifecycle,
			continuationDone,
			cardDeliveryDone,
			messageBot,
			approvals,
			resourceAccess,
			leaseHeartbeat,
			runtimeLease,
		)
		if releaseErr != nil && runtimeLease != nil {
			feishuLog.Warn(context.Background(), "release feishu account runtime lease failed account=%s owner_ref=%s: %v",
				runtimeLease.accountID, shortResourceRef(runtimeLease.ownerID), releaseErr)
		}
		if leaseHeartbeatErr != nil {
			feishuLog.Error(context.Background(), "feishu account runtime lease heartbeat failed account=%s: %v", p.account.ID, leaseHeartbeatErr)
			if ctx.Err() == nil && (runErr == nil || errors.Is(runErr, context.Canceled)) {
				runErr = fmt.Errorf("maintain feishu account runtime lease for account %s: %w", p.account.Name, leaseHeartbeatErr)
			}
		}
	}()

	acc := p.account
	accountConfig, ok := p.config.Accounts[acc.Name]
	if !ok {
		return fmt.Errorf("platforms.feishu.accounts.%s is required", acc.Name)
	}
	creds := feishu.CredentialsFromConfig(accountConfig)
	if creds.AppID == "" {
		return fmt.Errorf("feishu account %s credentials app_id is required", acc.Name)
	}
	if creds.AppSecret == "" {
		return fmt.Errorf("feishu account %s credentials app_secret is required", acc.Name)
	}
	docsEnabled := docsToolsEnabled(p.config.Tools)
	workflowResumer, err := workflowResumerForDocs(handler, docsEnabled)
	if err != nil {
		return fmt.Errorf("initialize feishu Docs workflows for account %s: %w", acc.Name, err)
	}
	baseURL := accountConfig.BaseURL

	sdkLogLevel := feishuSDKLogLevel(p.level)
	sdkLog := newSDKLevelLogger(sdkLogLevel, feishuSDKLog)
	restClient := newRESTClient(creds, baseURL, accountConfig.OAuthBaseURL, sdkLogLevel, sdkLog)
	botOpenID, err := fetchBotOpenID(ctx, restClient)
	if err != nil {
		return fmt.Errorf("resolve feishu bot identity for account %s: %w", acc.Name, err)
	}
	if p.store == nil {
		return fmt.Errorf("feishu account runtime lease requires a Feishu store")
	}
	runtimeLease, err = acquireFeishuAccountRuntimeLease(p.store, acc.ID, feishuAccountRuntimeLeaseOptions{})
	if err != nil {
		if errors.Is(err, store.ErrFeishuAccountRuntimeLeaseHeld) {
			return fmt.Errorf("feishu account %s is already active in another LingoBridge runtime: %w", acc.Name, err)
		}
		return fmt.Errorf("acquire feishu account runtime lease for account %s: %w", acc.Name, err)
	}
	leaseHeartbeat = runtimeLease.startHeartbeat(func() {
		// A lost account lease is not an orderly shutdown. Cancel both ordinary
		// runtime work and ownership-bound admitted side effects immediately.
		cancelOwnership()
		cancelLifecycle()
	})
	sender := &sdkSender{client: restClient}
	var cards CardService
	var operationApprovals feishutools.OperationApprovalService
	var docxAppendCipher *feishutools.DocxAppendEnvelopeCipher
	if docsEnabled {
		docxAppendCipher, err = feishutools.NewDocxAppendEnvelopeCipher(creds.AppSecret, acc.ID)
		if err != nil {
			return fmt.Errorf("initialize feishu docx append recovery encryption for account %s: %w", acc.Name, err)
		}
		cards, err = newCardService(sender)
		if err != nil {
			return fmt.Errorf("initialize feishu cards for account %s: %w", acc.Name, err)
		}
		resourceAccess, err = newResourceAccessManager(lifecycleCtx, p.store, restClient, acc, botOpenID, cards, resourceAccessOAuthConfig{
			ClientID:              creds.AppID,
			BaseURL:               accountConfig.OAuthBaseURL,
			CallbackURL:           accountConfig.OAuthCallbackURL,
			CallbackListenAddress: accountConfig.OAuthCallbackListenAddress,
			CredentialSecret:      creds.AppSecret,
		})
		if err != nil {
			return fmt.Errorf("initialize feishu resource access for account %s: %w", acc.Name, err)
		}
		if err := resourceAccess.recoverPersistedRequests(lifecycleCtx); err != nil {
			return fmt.Errorf("recover feishu resource access for account %s: %w", acc.Name, err)
		}
		if err := resourceAccess.startFeishuOAuthRefreshAttemptCleanup(); err != nil {
			return fmt.Errorf("start feishu OAuth refresh attempt cleanup for account %s: %w", acc.Name, err)
		}
	}
	if docsOperationApprovalRequired(p.config.Tools) {
		approvals, err = newOperationApprovalService(lifecycleCtx, p.store, acc, cards)
		if err != nil {
			return fmt.Errorf("initialize feishu tool approvals for account %s: %w", acc.Name, err)
		}
		operationApprovals = approvals
	}
	toolRuntime := newFeishuToolRuntime(restClient, p.store, acc.ID, p.config.Tools, operationApprovals, resourceAccess, docxAppendCipher, runtimeLease.ownerID)
	tools := toolRuntime.Tools()
	if approvals != nil {
		if err := registerApprovalExecutors(approvals, tools); err != nil {
			return fmt.Errorf("register feishu tool approval executors for account %s: %w", acc.Name, err)
		}
		if err := approvals.recoverPersistedApprovals(lifecycleCtx); err != nil {
			return fmt.Errorf("recover feishu tool approvals for account %s: %w", acc.Name, err)
		}
	}
	if err := toolRuntime.RecoverDocxAppendOperations(lifecycleCtx); err != nil {
		return fmt.Errorf("recover feishu docx append operations for account %s: %w", acc.Name, err)
	}
	var cardWorker *feishuCardDeliveryWorker
	var continuationWorker *workflowContinuationWorker
	if docsEnabled {
		cardWorker, err = newFeishuCardDeliveryWorker(p.store, cards, resourceAccess, acc)
		if err != nil {
			return fmt.Errorf("initialize feishu card delivery worker for account %s: %w", acc.Name, err)
		}
		continuationWorker, err = newWorkflowContinuationWorker(p.store, workflowResumer, sender, cards, acc, tools)
		if err != nil {
			return fmt.Errorf("initialize feishu workflow continuation worker for account %s: %w", acc.Name, err)
		}
	}
	if cardWorker != nil {
		done := make(chan struct{})
		cardDeliveryDone = done
		go func() {
			defer close(done)
			cardWorker.Run(lifecycleCtx)
		}()
	}
	if continuationWorker != nil {
		done := make(chan struct{})
		continuationDone = done
		go func() {
			defer close(done)
			continuationWorker.Run(lifecycleCtx)
		}()
	}
	if names := toolNames(tools); len(names) > 0 {
		feishuLog.Info(ctx, "registered tools for account %s (%s): %s", acc.Name, acc.ID, strings.Join(names, ", "))
	} else {
		feishuLog.Debug(ctx, "no tools registered for account %s (%s)", acc.Name, acc.ID)
	}
	messageBot = &bot{
		handler:       handler,
		sender:        sender,
		tools:         tools,
		account:       acc,
		botOpenID:     botOpenID,
		eventCommands: map[string][]string{},
		approvals:     approvals,
		cards:         cards,
		deduper:       newEventDeduper(defaultFeishuDedupeTTL),
		runCtx:        lifecycleCtx,
		reactionDelay: feishuReactionClearDelay,
	}
	b := messageBot

	d := dispatcher.NewEventDispatcher("", "")
	d.Config.Logger = sdkLog
	d, registeredEvents, err := b.configureEventHandlers(d, p.config.Events)
	if err != nil {
		return err
	}
	opts := []larkws.ClientOption{
		larkws.WithEventHandler(d),
		larkws.WithLogger(sdkLog),
		larkws.WithLogLevel(sdkLogLevel),
		larkws.WithOnReady(func() {
			feishuLog.Info(ctx, "long connection ready for account %s (%s)", acc.Name, acc.ID)
		}),
		larkws.WithOnError(func(err error) {
			feishuLog.Error(ctx, "long connection error account=%s: %v", acc.Name, err)
		}),
	}
	if domain := strings.TrimRight(strings.TrimSpace(baseURL), "/"); domain != "" {
		opts = append(opts, larkws.WithDomain(domain))
	}
	wsClient := larkws.NewClient(creds.AppID, creds.AppSecret, opts...)
	oauthServer, err := startResourceAccessOAuthServer(lifecycleCtx, resourceAccess)
	if err != nil {
		return fmt.Errorf("start feishu OAuth callback server for account %s: %w", acc.Name, err)
	}
	if oauthServer != nil {
		defer func() {
			if closeErr := oauthServer.Close(); closeErr != nil {
				feishuLog.Warn(context.Background(), "close feishu OAuth callback server account=%s: %v", acc.ID, closeErr)
			}
		}()
	}
	feishuLog.Info(ctx, "registered events for account %s (%s): %s", acc.Name, acc.ID, strings.Join(registeredEvents, ", "))
	feishuLog.Info(ctx, "starting for account %s (%s)", acc.Name, acc.ID)
	return runClient(lifecycleCtx, wsClient, oauthServer)
}

type feishuToolRuntime struct {
	tools []tooltypes.Tool
	docs  *feishutools.DocsRuntime
}

func newFeishuToolRuntime(client *lark.Client, st *store.Store, accountID string, cfg feishutools.Config, approvals feishutools.OperationApprovalService, resourceAccess feishutools.ResourceAccessController, appendCipher *feishutools.DocxAppendEnvelopeCipher, appendExecutionOwner string) feishuToolRuntime {
	docs := feishutools.NewDocsRuntime(client, st, accountID, cfg, approvals, resourceAccess, appendCipher, appendExecutionOwner)
	tools := feishutools.NewChatHistoryTools(client, cfg)
	tools = append(tools, feishutools.NewDocsResourceAccessTools(resourceAccess, cfg)...)
	tools = append(tools, docs.Tools()...)
	tools = append(tools, feishutools.NewDocsFolderTools(client, st, accountID, cfg, resourceAccess)...)
	tools = append(tools, feishutools.NewLiteLLMAccountTools(client, cfg)...)
	return feishuToolRuntime{tools: tools, docs: docs}
}

func (r feishuToolRuntime) Tools() []tooltypes.Tool {
	return r.tools
}

func (r feishuToolRuntime) RecoverDocxAppendOperations(ctx context.Context) error {
	if r.docs == nil {
		return nil
	}
	return r.docs.RecoverDocxAppendOperations(ctx)
}

func docsToolsEnabled(cfg feishutools.Config) bool {
	cfg = feishutools.NormalizeConfig(cfg)
	return cfg.Docs.Enabled
}

func workflowResumerForDocs(handler core.Handler, docsEnabled bool) (core.WorkflowResumer, error) {
	if !docsEnabled {
		return nil, nil
	}
	resumer, ok := handler.(core.WorkflowResumer)
	if !ok {
		return nil, errors.New("feishu Docs workflows require handler workflow resumption")
	}
	return resumer, nil
}

func docsOperationApprovalRequired(cfg feishutools.Config) bool {
	cfg = feishutools.NormalizeConfig(cfg)
	return cfg.Docs.Enabled && cfg.Docs.AllowWrite
}

func registerApprovalExecutors(approvals *operationApprovalService, tools []tooltypes.Tool) error {
	registered := 0
	for _, tool := range tools {
		executor, ok := tool.(feishutools.OperationApprovalExecutor)
		if !ok {
			continue
		}
		policy := executor.OperationApprovalPolicy()
		if strings.TrimSpace(policy.ToolName) == "" || strings.TrimSpace(policy.ActionKey) == "" {
			continue
		}
		if err := approvals.registerExecutor(executor); err != nil {
			return err
		}
		registered++
	}
	if registered == 0 {
		return fmt.Errorf("no approval-gated tools were registered")
	}
	return nil
}

func toolNames(tools []tooltypes.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Spec().Name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func runClient(ctx context.Context, client interface {
	starter
	closer
}, oauthServer *oauthCallbackServer) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Start(ctx)
	}()
	oauthErrCh := (<-chan error)(nil)
	if oauthServer != nil {
		oauthErrCh = oauthServer.Errors()
	}
	for {
		select {
		case err := <-errCh:
			return err
		case err, ok := <-oauthErrCh:
			client.Close()
			if !ok || err == nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("feishu OAuth callback server stopped unexpectedly")
			}
			return fmt.Errorf("feishu OAuth callback server: %w", err)
		case <-ctx.Done():
			client.Close()
			return nil
		}
	}
}

func newRESTClient(creds feishu.Credentials, baseURL, oauthBaseURL string, level larkcore.LogLevel, logger larkcore.Logger) *lark.Client {
	opts := []lark.ClientOptionFunc{
		lark.WithLogger(logger),
		lark.WithLogLevel(level),
	}
	if baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/"); baseURL != "" {
		opts = append(opts, lark.WithOpenBaseUrl(baseURL))
	}
	if oauthBaseURL = strings.TrimRight(strings.TrimSpace(oauthBaseURL), "/"); oauthBaseURL != "" {
		opts = append(opts, lark.WithOAuthBaseUrl(oauthBaseURL))
	}
	return lark.NewClient(creds.AppID, creds.AppSecret, opts...)
}

type botInfoClient interface {
	Get(ctx context.Context, httpPath string, body interface{}, accessTokenType larkcore.AccessTokenType, options ...larkcore.RequestOptionFunc) (*larkcore.ApiResp, error)
}

func fetchBotOpenID(ctx context.Context, client botInfoClient) (string, error) {
	resp, err := client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("get bot info: %w", err)
	}
	if resp == nil {
		return "", fmt.Errorf("get bot info: empty response")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get bot info: status=%d", resp.StatusCode)
	}

	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		return "", fmt.Errorf("parse bot info: %w", err)
	}
	if body.Code != 0 {
		return "", fmt.Errorf("get bot info: code=%d msg=%s", body.Code, body.Msg)
	}
	openID := strings.TrimSpace(body.Bot.OpenID)
	if openID == "" {
		return "", fmt.Errorf("get bot info: missing bot.open_id")
	}
	return openID, nil
}

type sdkLevelLogger struct {
	level larkcore.LogLevel
	next  larkcore.Logger
}

func newSDKLevelLogger(level larkcore.LogLevel, next larkcore.Logger) larkcore.Logger {
	return sdkLevelLogger{level: level, next: next}
}

func (l sdkLevelLogger) Debug(ctx context.Context, args ...interface{}) {
	if l.next != nil && l.level <= larkcore.LogLevelDebug && !sdkDebugContainsRawPayload(args) {
		l.next.Debug(ctx, args...)
	}
}

func sdkDebugContainsRawPayload(args []interface{}) bool {
	message := strings.ToLower(fmt.Sprint(args...))
	return strings.Contains(message, "payload:") ||
		strings.Contains(message, "event request:") ||
		strings.Contains(message, "card request:")
}

func (l sdkLevelLogger) Info(ctx context.Context, args ...interface{}) {
	if l.next != nil && l.level <= larkcore.LogLevelInfo {
		l.next.Info(ctx, args...)
	}
}

func (l sdkLevelLogger) Warn(ctx context.Context, args ...interface{}) {
	if l.next != nil && l.level <= larkcore.LogLevelWarn {
		l.next.Warn(ctx, args...)
	}
}

func (l sdkLevelLogger) Error(ctx context.Context, args ...interface{}) {
	if l.next != nil && l.level <= larkcore.LogLevelError {
		l.next.Error(ctx, args...)
	}
}

func feishuSDKLogLevel(level logging.Level) larkcore.LogLevel {
	switch level {
	case logging.All:
		return larkcore.LogLevelDebug
	case logging.Warn:
		return larkcore.LogLevelWarn
	case logging.Error:
		return larkcore.LogLevelError
	default:
		return larkcore.LogLevelInfo
	}
}
