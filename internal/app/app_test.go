package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"lingobridge/internal/config"
	"lingobridge/internal/core"
	"lingobridge/internal/logging"
	"lingobridge/internal/platform"
	"lingobridge/internal/platform/feishu"
	githubplatform "lingobridge/internal/platform/github"
	wechatplatform "lingobridge/internal/platform/wechat"
	"lingobridge/internal/runner"
	"lingobridge/internal/store"
)

func TestCmdAccountNewFeishuSavesConfigAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)

	err := cmdAccountNew([]string{
		"feishu",
		"--name", "fsbot",
		"--app-id", "cli_xxx",
		"--app-secret", "secret",
	})
	if err != nil {
		t.Fatalf("cmdAccountNew returned error: %v", err)
	}

	st, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts returned error: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("sqlite feishu accounts = %#v, want none", accounts)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, st, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	feishuAccount, ok, err := feishu.ResolveAccountConfig(platformCtx, "fsbot")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if !ok || feishuAccount.AppID != "cli_xxx" || feishuAccount.AppSecret != "secret" {
		t.Fatalf("feishu account = %#v ok=%v", feishuAccount, ok)
	}
}

func TestCmdAccountNewFeishuRequiresCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := cmdAccountNew([]string{"feishu", "--name", "fsbot"})
	if err == nil {
		t.Fatal("cmdAccountNew returned nil error, want missing credentials error")
	}
}

func TestCmdAccountNewFeishuAliasSavesConfigAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)

	err := cmdAccountNew([]string{
		"飞书",
		"--name", "fsbot",
		"--app-id", "cli_alias",
		"--app-secret", "secret",
	})
	if err != nil {
		t.Fatalf("cmdAccountNew returned error: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	feishuAccount, ok, err := feishu.ResolveAccountConfig(platformCtx, "fsbot")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if !ok || feishuAccount.AppID != "cli_alias" {
		t.Fatalf("feishu account = %#v ok=%v", feishuAccount, ok)
	}
}

func TestCmdAccountNewGitHubSavesConfigAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)

	err := cmdAccountNew([]string{
		"github",
		"--name", "reviewer",
		"--app-id", "123456",
		"--installation-id", "987654",
		"--private-key-path", "/tmp/github-app.pem",
		"--repo", "owner/repo",
	})
	if err != nil {
		t.Fatalf("cmdAccountNew returned error: %v", err)
	}

	if accounts := listTestAccounts(t, store.PlatformGitHub); len(accounts) != 0 {
		t.Fatalf("sqlite github accounts = %#v, want none", accounts)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformGitHub, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	githubAccount, ok, err := githubplatform.ResolveAccountConfig(platformCtx, "reviewer")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if !ok || githubAccount.InstallationID != "987654" || len(githubAccount.Repositories) != 1 {
		t.Fatalf("github account = %#v ok=%v", githubAccount, ok)
	}
}

func TestCmdAccountNewWeixinAliasUsesPlatformDefinition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	registry := newFakeAccountNewRegistry(t)

	err := cmdAccountNewWithRegistry([]string{"weixin", "--name", "wxbot"}, registry)
	if err != nil {
		t.Fatalf("cmdAccountNewWithRegistry returned error: %v", err)
	}

	st, err := store.Open(store.PlatformWeChat)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()

	acc, err := st.GetAccount("wechat:test")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if acc.Name != "wxbot" || acc.Platform != store.PlatformWeChat {
		t.Fatalf("account = %#v", acc)
	}
}

func TestCmdAccountNewRejectsOldPlatformFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := cmdAccountNew([]string{"--platform", "feishu", "--name", "fsbot"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("cmdAccountNew error = %v, want ErrUsage", err)
	}
}

func TestCmdAccountNewUnknownPlatform(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	err := cmdAccountNew([]string{"unknown"})
	if err == nil {
		t.Fatal("cmdAccountNew returned nil error, want unsupported platform error")
	}
}

func TestCmdAccountDeleteUniqueNameDeletesAndNotesWithoutRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	saveTestAccount(t, store.Account{
		ID:              "wechat:test",
		Name:            "default",
		Platform:        store.PlatformWeChat,
		Token:           "token",
		BaseURL:         "base",
		UserID:          "user",
		CredentialsJSON: "{}",
		Enabled:         true,
	})
	saveTestSyncCursor(t, store.PlatformWeChat, "wechat:test", "buf")

	out, err := captureStdout(t, func() error {
		return cmdAccountDelete([]string{"default"})
	})
	if err != nil {
		t.Fatalf("cmdAccountDelete returned error: %v", err)
	}
	if !strings.Contains(out, "Deleted account: wechat/default") {
		t.Fatalf("output = %q, want deleted account", out)
	}
	if !strings.Contains(out, "Note: No running lingobridge process found") {
		t.Fatalf("output = %q, want no-run note", out)
	}
	if accounts := listTestAccounts(t, store.PlatformWeChat); len(accounts) != 0 {
		t.Fatalf("wechat accounts = %#v, want empty after delete", accounts)
	}
	if got := loadTestSyncCursor(t, store.PlatformWeChat, "wechat:test"); got != "" {
		t.Fatalf("wechat sync cursor = %q, want deleted", got)
	}
}

func TestCmdAccountDeleteAmbiguousNameRequiresPlatformName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	saveTestAccount(t, store.Account{
		ID:              "wechat:test",
		Name:            "default",
		Platform:        store.PlatformWeChat,
		CredentialsJSON: "{}",
		Enabled:         true,
	})
	upsertTestFeishuAccountConfig(t, "default", "cli_xxx")

	err := cmdAccountDelete([]string{"default"})
	if err == nil {
		t.Fatal("cmdAccountDelete returned nil error, want ambiguous account error")
	}
	for _, want := range []string{"ambiguous", "feishu/default", "wechat/default"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	if accounts := listTestAccounts(t, store.PlatformWeChat); len(accounts) != 1 {
		t.Fatalf("wechat accounts = %#v, want unchanged", accounts)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if _, ok, err := feishu.ResolveAccountConfig(platformCtx, "default"); err != nil || !ok {
		t.Fatalf("feishu config account ok=%v err=%v, want unchanged", ok, err)
	}
}

func TestCmdAccountDeletePlatformSelectorDeletesMatchingAccountAndFeishuConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	saveTestAccount(t, store.Account{
		ID:              "wechat:test",
		Name:            "default",
		Platform:        store.PlatformWeChat,
		CredentialsJSON: "{}",
		Enabled:         true,
	})
	upsertTestFeishuAccountConfig(t, "default", "cli_xxx")
	saveTestSyncCursor(t, store.PlatformFeishu, "feishu:cli_xxx", `{"old":true}`)

	out, err := captureStdout(t, func() error {
		return cmdAccountDelete([]string{"feishu/default"})
	})
	if err != nil {
		t.Fatalf("cmdAccountDelete returned error: %v", err)
	}
	if !strings.Contains(out, "Deleted account: feishu/default") {
		t.Fatalf("output = %q, want feishu/default delete", out)
	}
	if got := loadTestSyncCursor(t, store.PlatformFeishu, "feishu:cli_xxx"); got != "" {
		t.Fatalf("feishu sync cursor = %q, want deleted", got)
	}
	if accounts := listTestAccounts(t, store.PlatformWeChat); len(accounts) != 1 || accounts[0].Name != "default" {
		t.Fatalf("wechat accounts = %#v, want default account preserved", accounts)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	_, ok, err := feishu.ResolveAccountConfig(platformCtx, "default")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if ok {
		t.Fatal("feishu account config still exists after account delete")
	}
}

func TestCmdAccountListShowsPlatformName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	saveTestAccount(t, store.Account{
		ID:              "wechat:test",
		Name:            "default",
		Platform:        store.PlatformWeChat,
		CredentialsJSON: "{}",
		Enabled:         true,
	})
	upsertTestFeishuAccountConfig(t, "default", "cli_xxx")

	out, err := captureStdout(t, func() error {
		return cmdAccountList(nil)
	})
	if err != nil {
		t.Fatalf("cmdAccountList returned error: %v", err)
	}
	for _, want := range []string{"feishu/default", "wechat/default"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "default [feishu]") || strings.Contains(out, "default [wechat]") {
		t.Fatalf("output = %q, want platform/name format", out)
	}
}

func TestCmdAccountListShowsConfigGitHubAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	upsertTestGitHubAccountConfig(t, "reviewer", "987654")

	out, err := captureStdout(t, func() error {
		return cmdAccountList(nil)
	})
	if err != nil {
		t.Fatalf("cmdAccountList returned error: %v", err)
	}
	if !strings.Contains(out, "github/reviewer") || !strings.Contains(out, "github:987654") {
		t.Fatalf("output = %q, want github config account", out)
	}
}

func TestCmdRunStartsConfigGitHubAccountAndReportsMissingMCP(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "lb-gh-run-")
	if err != nil {
		t.Fatalf("MkdirTemp returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	writeTestConfig(t)
	upsertTestGitHubAccountConfig(t, "reviewer", "987654")

	err = cmdRun([]string{"--account", "reviewer"})
	if err == nil {
		t.Fatal("cmdRun returned nil error, want missing github mcp config")
	}
	for _, want := range []string{"monitor exited", "platform=github", "name=reviewer", "mcp.command"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("cmdRun error = %q, want %q", err.Error(), want)
		}
	}
}

func TestCmdAccountDeleteGitHubDeletesConfigAndCursor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	upsertTestGitHubAccountConfig(t, "reviewer", "987654")
	saveTestSyncCursor(t, store.PlatformGitHub, "github:987654", `{"old":true}`)

	out, err := captureStdout(t, func() error {
		return cmdAccountDelete([]string{"github/reviewer"})
	})
	if err != nil {
		t.Fatalf("cmdAccountDelete returned error: %v", err)
	}
	if !strings.Contains(out, "Deleted account: github/reviewer") {
		t.Fatalf("output = %q, want github/reviewer delete", out)
	}
	if got := loadTestSyncCursor(t, store.PlatformGitHub, "github:987654"); got != "" {
		t.Fatalf("github sync cursor = %q, want deleted", got)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformGitHub, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	_, ok, err := githubplatform.ResolveAccountConfig(platformCtx, "reviewer")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if ok {
		t.Fatal("github account config still exists after account delete")
	}
}

func TestParseRunOptionsDefaultsLogLevelToInfo(t *testing.T) {
	opts, err := parseRunOptions(nil)
	if err != nil {
		t.Fatalf("parseRunOptions returned error: %v", err)
	}
	if opts.logLevel != logging.Info {
		t.Fatalf("logLevel = %v, want info", opts.logLevel)
	}
}

func TestParseRunOptionsAcceptsVerboseDebug(t *testing.T) {
	opts, err := parseRunOptions([]string{"--verbose", "debug", "--account", "fsbot"})
	if err != nil {
		t.Fatalf("parseRunOptions returned error: %v", err)
	}
	if opts.logLevel != logging.Debug || opts.targetAccount != "fsbot" {
		t.Fatalf("options = %#v, want debug level and target account", opts)
	}
}

func TestParseRunOptionsAcceptsVerboseAll(t *testing.T) {
	opts, err := parseRunOptions([]string{"--verbose", "all"})
	if err != nil {
		t.Fatalf("parseRunOptions returned error: %v", err)
	}
	if opts.logLevel != logging.All {
		t.Fatalf("logLevel = %v, want all", opts.logLevel)
	}
}

func TestParseRunOptionsRejectsInvalidVerbose(t *testing.T) {
	if _, err := parseRunOptions([]string{"--verbose", "noisy"}); err == nil {
		t.Fatal("parseRunOptions returned nil error, want invalid verbose error")
	}
}

func TestRuntimeStateEnablesTextStreamingOnlyForFeishu(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	wechatStore, err := store.Open(store.PlatformWeChat)
	if err != nil {
		t.Fatalf("Open wechat returned error: %v", err)
	}
	defer wechatStore.Close()
	feishuStore, err := store.Open(store.PlatformFeishu)
	if err != nil {
		t.Fatalf("Open feishu returned error: %v", err)
	}
	defer feishuStore.Close()

	state, err := newRuntimeState(map[string]*store.Store{
		store.PlatformWeChat: wechatStore,
		store.PlatformFeishu: feishuStore,
	}, cfg)
	if err != nil {
		t.Fatalf("newRuntimeState returned error: %v", err)
	}

	_, wechatRuntime, ok := state.snapshot(store.PlatformWeChat)
	if !ok {
		t.Fatal("wechat runtime not found")
	}
	if wechatRuntime.handler.EnableTextStreaming {
		t.Fatal("wechat EnableTextStreaming = true, want false")
	}
	if wechatRuntime.handler.TextChunkLimit != wechatplatform.TextChunkLimit {
		t.Fatalf("wechat TextChunkLimit = %d, want %d", wechatRuntime.handler.TextChunkLimit, wechatplatform.TextChunkLimit)
	}
	if wechatRuntime.handler.ToolProvider != state.mcpHost {
		t.Fatal("wechat ToolProvider is not the runtime MCP host")
	}
	_, feishuRuntime, ok := state.snapshot(store.PlatformFeishu)
	if !ok {
		t.Fatal("feishu runtime not found")
	}
	if !feishuRuntime.handler.EnableTextStreaming {
		t.Fatal("feishu EnableTextStreaming = false, want true")
	}
	if feishuRuntime.handler.TextChunkLimit != feishu.TextChunkLimit {
		t.Fatalf("feishu TextChunkLimit = %d, want %d", feishuRuntime.handler.TextChunkLimit, feishu.TextChunkLimit)
	}
	if feishuRuntime.handler.ToolProvider != state.mcpHost {
		t.Fatal("feishu ToolProvider is not the runtime MCP host")
	}
	if feishuRuntime.handler.Workflows != feishuStore {
		t.Fatal("feishu workflow continuation manager is not the platform store")
	}
	if err := state.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

func TestWaitRunDoneReturnsFatalMonitorError(t *testing.T) {
	wantErr := errors.New("bad config")
	fatal := make(chan error, 1)
	fatal <- formatMonitorExitError(runner.MonitorExit{
		Account: store.Account{
			ID:       "feishu:cli_xxx",
			Name:     "default",
			Platform: store.PlatformFeishu,
		},
		Err: wantErr,
	})

	err := waitRunDone(context.Background(), fatal)
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitRunDone error = %v, want wrapped fatal monitor error", err)
	}
	for _, want := range []string{"monitor exited", "platform=feishu", "name=default", "id=feishu:cli_xxx"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("waitRunDone error = %q, want %q", err.Error(), want)
		}
	}
}

func TestWaitRunDoneReturnsNilOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitRunDone(ctx, make(chan error)); err != nil {
		t.Fatalf("waitRunDone returned error: %v", err)
	}
}

func TestEnsureConfigInitializedCreatesFirstModelAsDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := strings.NewReader("first\nopenai\nhttps://api.example.com/v1\nkey\nmodel\n\n")
	var out bytes.Buffer

	if err := ensureConfigInitialized(input, &out); err != nil {
		t.Fatalf("ensureConfigInitialized returned error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.LLM.DefaultModel != "first" || !cfg.LLM.HasModel("first") {
		t.Fatalf("llm config = %#v", cfg.LLM)
	}
}

func TestEnsureConfigInitializedRepromptsInvalidEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := strings.NewReader("first\nopenai\nhttps://api.example.com/v1\nkey\nmodel\nresponse\nresponses\n128000\n")
	var out bytes.Buffer

	if err := ensureConfigInitialized(input, &out); err != nil {
		t.Fatalf("ensureConfigInitialized returned error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.LLM.Models["first"].Endpoint != "responses" {
		t.Fatalf("endpoint = %q, want responses", cfg.LLM.Models["first"].Endpoint)
	}
	if !strings.Contains(out.String(), "注意不是 response") || !strings.Contains(out.String(), "复数 responses") {
		t.Fatalf("prompt output = %q, want responses guidance", out.String())
	}
}

func TestCmdModelAddWritesConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTestConfig(t)
	var out bytes.Buffer

	err := cmdModelAdd([]string{
		"gpt4o",
		"--provider", "openai",
		"--base-url", "https://api.openai.com/v1",
		"--api-key", "key",
		"--id", "gpt-4o",
		"--endpoint", "responses",
		"--context-window", "128000",
		"--default",
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("cmdModelAdd returned error: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.LLM.DefaultModel != "gpt4o" || cfg.LLM.Models["gpt4o"].Endpoint != "responses" {
		t.Fatalf("llm config = %#v", cfg.LLM)
	}
}

type fakeRuntimePlatform struct{}

func (fakeRuntimePlatform) Run(ctx context.Context, handler core.Handler) error {
	return nil
}

func newFakeAccountNewRegistry(t *testing.T) *platform.Registry {
	t.Helper()
	registry := platform.NewRegistry()
	err := registry.Register(platform.Definition{
		ID:              store.PlatformWeChat,
		Aliases:         []string{"weixin", "微信"},
		AccountNewUsage: "lingobridge account new weixin [--name <name>]",
		ParseAccountNewFlags: func(args []string, io platform.AccountNewIO) (platform.AccountNewOptions, error) {
			name := "default"
			for i := 0; i < len(args); i++ {
				if args[i] == "--name" && i+1 < len(args) {
					name = args[i+1]
					i++
				}
			}
			return platform.AccountNewOptions{Name: name}, nil
		},
		CreateOrUpdateAccount: func(ctx platform.AccountNewContext, opts platform.AccountNewOptions) error {
			return ctx.Platform.DataStore().SaveAccount(store.Account{
				ID:              "wechat:test",
				Name:            opts.Name,
				Platform:        store.PlatformWeChat,
				CredentialsJSON: "{}",
				Enabled:         true,
			})
		},
		ListAccounts: func(ctx platform.AccountListContext) ([]store.Account, error) {
			return ctx.Platform.DataStore().ListAccounts()
		},
		DeleteAccount: func(ctx platform.AccountDeleteContext) error {
			return ctx.Platform.DataStore().DeleteAccount(ctx.Account.ID)
		},
		NewRuntimePlatform: func(ctx platform.RuntimeContext) (core.Platform, error) {
			return fakeRuntimePlatform{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	return registry
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe returned error: %v", err)
	}
	os.Stdout = w
	errRun := fn()
	if err := w.Close(); err != nil {
		os.Stdout = old
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(out), errRun
}

func saveTestAccount(t *testing.T, account store.Account) {
	t.Helper()
	st, err := store.Open(account.Platform)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()
	if err := st.SaveAccount(account); err != nil {
		t.Fatalf("SaveAccount returned error: %v", err)
	}
}

func listTestAccounts(t *testing.T, platformID string) []store.Account {
	t.Helper()
	st, err := store.Open(platformID)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()
	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts returned error: %v", err)
	}
	return accounts
}

func upsertTestFeishuAccountConfig(t *testing.T, name, appID string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, config.Save)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := feishu.UpsertAccountConfig(platformCtx, name, feishu.AccountConfig{
		AppID:     appID,
		AppSecret: "secret",
	}); err != nil {
		t.Fatalf("UpsertAccountConfig returned error: %v", err)
	}
}

func upsertTestGitHubAccountConfig(t *testing.T, name, installationID string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	platformCtx, err := core.NewPlatformContext(store.PlatformGitHub, &cfg, nil, config.Save)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := githubplatform.UpsertAccountConfig(platformCtx, name, githubplatform.AccountConfig{
		AppID:          "123456",
		InstallationID: installationID,
		PrivateKeyPath: "/tmp/github-app.pem",
		Repositories:   []string{"owner/repo"},
	}); err != nil {
		t.Fatalf("UpsertAccountConfig returned error: %v", err)
	}
}

func saveTestSyncCursor(t *testing.T, platformID, accountID, buf string) {
	t.Helper()
	st, err := store.Open(platformID)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()
	if err := st.SaveSyncBuf(accountID, buf); err != nil {
		t.Fatalf("SaveSyncBuf returned error: %v", err)
	}
}

func loadTestSyncCursor(t *testing.T, platformID, accountID string) string {
	t.Helper()
	st, err := store.Open(platformID)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()
	buf, err := st.GetSyncBuf(accountID)
	if err != nil {
		t.Fatalf("GetSyncBuf returned error: %v", err)
	}
	return buf
}

func writeTestConfig(t *testing.T) {
	t.Helper()
	cfg := config.DefaultConfig()
	if err := config.AddModel(&cfg, "deepseek", config.LLMModelConfig{
		Provider: "openai",
		BaseURL:  "https://api.deepseek.com/v1",
		APIKey:   "key",
		ID:       "deepseek-chat",
	}, true); err != nil {
		t.Fatalf("AddModel returned error: %v", err)
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
}
