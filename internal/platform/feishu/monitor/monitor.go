package monitor

import (
	"context"
	"encoding/json"
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

func (p *Platform) Run(ctx context.Context, handler core.Handler) error {
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
	baseURL := accountConfig.BaseURL

	sdkLogLevel := feishuSDKLogLevel(p.level)
	sdkLog := newSDKLevelLogger(sdkLogLevel, feishuSDKLog)
	restClient := newRESTClient(creds, baseURL, accountConfig.OAuthBaseURL, sdkLogLevel, sdkLog)
	botOpenID, err := fetchBotOpenID(ctx, restClient)
	if err != nil {
		return fmt.Errorf("resolve feishu bot identity for account %s: %w", acc.Name, err)
	}
	sender := &sdkSender{client: restClient}
	var approvals *approvalManager
	var resourceAccess *resourceAccessManager
	var cards CardService
	var approver feishutools.ApprovalRequester
	if docsToolsEnabled(p.config.Tools) {
		cards, err = newCardService(sender)
		if err != nil {
			return fmt.Errorf("initialize feishu cards for account %s: %w", acc.Name, err)
		}
		resourceAccess, err = newResourceAccessManager(ctx, p.store, restClient, acc, botOpenID, cards, resourceAccessOAuthConfig{
			ClientID:              creds.AppID,
			BaseURL:               accountConfig.OAuthBaseURL,
			CallbackURL:           accountConfig.OAuthCallbackURL,
			CallbackListenAddress: accountConfig.OAuthCallbackListenAddress,
			CredentialSecret:      creds.AppSecret,
		})
		if err != nil {
			return fmt.Errorf("initialize feishu resource access for account %s: %w", acc.Name, err)
		}
		if err := resourceAccess.recoverPersistedRequests(ctx); err != nil {
			return fmt.Errorf("recover feishu resource access for account %s: %w", acc.Name, err)
		}
	}
	if docsCreateApprovalRequired(p.config.Tools) {
		approvals, err = newApprovalManager(ctx, p.store, acc, cards)
		if err != nil {
			return fmt.Errorf("initialize feishu tool approvals for account %s: %w", acc.Name, err)
		}
		if err := approvals.recoverPersistedApprovals(ctx); err != nil {
			return fmt.Errorf("recover feishu tool approvals for account %s: %w", acc.Name, err)
		}
		approver = approvals
	}
	tools := newFeishuTools(restClient, p.store, acc.ID, p.config.Tools, approver, resourceAccess)
	if approvals != nil {
		if err := registerApprovalExecutors(approvals, tools); err != nil {
			return fmt.Errorf("register feishu tool approval executors for account %s: %w", acc.Name, err)
		}
	}
	if docsToolsEnabled(p.config.Tools) {
		if resumer, ok := handler.(core.WorkflowResumer); ok {
			worker, workerErr := newWorkflowContinuationWorker(p.store, resumer, sender, acc, tools)
			if workerErr != nil {
				return fmt.Errorf("initialize feishu workflow continuation worker for account %s: %w", acc.Name, workerErr)
			}
			workerCtx, stopWorker := context.WithCancel(ctx)
			defer stopWorker()
			go worker.Run(workerCtx)
		} else {
			feishuLog.Warn(ctx, "feishu workflow continuation worker disabled because handler does not support resumption account=%s", acc.ID)
		}
	}
	if names := toolNames(tools); len(names) > 0 {
		feishuLog.Info(ctx, "registered tools for account %s (%s): %s", acc.Name, acc.ID, strings.Join(names, ", "))
	} else {
		feishuLog.Debug(ctx, "no tools registered for account %s (%s)", acc.Name, acc.ID)
	}
	b := &bot{
		handler:       handler,
		sender:        sender,
		tools:         tools,
		account:       acc,
		botOpenID:     botOpenID,
		eventCommands: map[string][]string{},
		approvals:     approvals,
		cards:         cards,
		deduper:       newEventDeduper(defaultFeishuDedupeTTL),
		runCtx:        ctx,
		reactionDelay: feishuReactionClearDelay,
	}

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
	oauthServer, err := startResourceAccessOAuthServer(ctx, resourceAccess)
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
	return runClient(ctx, wsClient, oauthServer)
}

func newFeishuTools(client *lark.Client, st *store.Store, accountID string, cfg feishutools.Config, approver feishutools.ApprovalRequester, resourceAccess feishutools.ResourceAccessController) []tooltypes.Tool {
	tools := feishutools.NewChatHistoryTools(client, cfg)
	tools = append(tools, feishutools.NewDocsResourceAccessTools(resourceAccess, cfg)...)
	tools = append(tools, feishutools.NewDocsTools(client, st, accountID, cfg, approver, resourceAccess)...)
	tools = append(tools, feishutools.NewDocsFolderTools(client, st, accountID, cfg, resourceAccess)...)
	tools = append(tools, feishutools.NewLiteLLMAccountTools(client, cfg)...)
	return tools
}

func docsToolsEnabled(cfg feishutools.Config) bool {
	cfg = feishutools.NormalizeConfig(cfg)
	return cfg.Docs.Enabled
}

func docsCreateApprovalRequired(cfg feishutools.Config) bool {
	cfg = feishutools.NormalizeConfig(cfg)
	return cfg.Docs.Enabled && cfg.Docs.AllowWrite
}

func registerApprovalExecutors(approvals *approvalManager, tools []tooltypes.Tool) error {
	registered := 0
	for _, tool := range tools {
		executor, ok := tool.(feishutools.ApprovalExecutor)
		if !ok || strings.TrimSpace(executor.ApprovalToolName()) == "" {
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
