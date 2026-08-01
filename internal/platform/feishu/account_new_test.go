package feishu

import (
	"bytes"
	"strings"
	"testing"

	"lingobridge/internal/config"
	"lingobridge/internal/core"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"

	"gopkg.in/yaml.v3"
)

func TestParseAccountNewFlagsPromptsForMissingCredentials(t *testing.T) {
	var out bytes.Buffer
	opts, err := ParseAccountNewFlags(nil, strings.NewReader("cli_prompt\nsecret_prompt\n\n"), &out)
	if err != nil {
		t.Fatalf("ParseAccountNewFlags returned error: %v", err)
	}
	if opts.Name != "default" || opts.AppID != "cli_prompt" || opts.AppSecret != "secret_prompt" || opts.BaseURL != "" {
		t.Fatalf("options = %#v", opts)
	}
	for _, want := range []string{"飞书 App ID", "飞书 App Secret", "飞书 API Base URL"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("prompt output = %q, want %q", out.String(), want)
		}
	}
}

func TestParseAccountNewFlagsPromptsOnlyMissingFields(t *testing.T) {
	var out bytes.Buffer
	opts, err := ParseAccountNewFlags(
		[]string{"--name", "fsbot", "--app-id", "cli_flag"},
		strings.NewReader("secret_prompt\nhttps://open.feishu.cn\n"),
		&out,
	)
	if err != nil {
		t.Fatalf("ParseAccountNewFlags returned error: %v", err)
	}
	if opts.Name != "fsbot" || opts.AppID != "cli_flag" || opts.AppSecret != "secret_prompt" || opts.BaseURL != "https://open.feishu.cn" {
		t.Fatalf("options = %#v", opts)
	}
	if strings.Contains(out.String(), "飞书 App ID") {
		t.Fatalf("prompt output = %q, did not want App ID prompt", out.String())
	}
	if !strings.Contains(out.String(), "飞书 App Secret") {
		t.Fatalf("prompt output = %q, want App Secret prompt", out.String())
	}
}

func TestParseAccountNewFlagsDoesNotPromptWhenRequiredFlagsProvided(t *testing.T) {
	var out bytes.Buffer
	opts, err := ParseAccountNewFlags(
		[]string{"--name", "fsbot", "--app-id", "cli_flag", "--app-secret", "secret_flag"},
		strings.NewReader(""),
		&out,
	)
	if err != nil {
		t.Fatalf("ParseAccountNewFlags returned error: %v", err)
	}
	if opts.Name != "fsbot" || opts.AppID != "cli_flag" || opts.AppSecret != "secret_flag" || opts.BaseURL != "" {
		t.Fatalf("options = %#v", opts)
	}
	if out.String() != "" {
		t.Fatalf("prompt output = %q, want no prompt", out.String())
	}
}

func TestNewAccountUsesParsedPromptCredentials(t *testing.T) {
	opts, err := ParseAccountNewFlags([]string{"--name", "fsbot"}, strings.NewReader("cli_prompt\nsecret_prompt\n\n"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseAccountNewFlags returned error: %v", err)
	}
	acc, err := NewAccount(opts.Name, opts.AppID, opts.AppSecret, opts.BaseURL)
	if err != nil {
		t.Fatalf("NewAccount returned error: %v", err)
	}
	if acc.Name != "fsbot" || acc.UserID != "cli_prompt" || acc.CredentialsJSON != "{}" {
		t.Fatalf("account = %#v", acc)
	}
}

func TestUpsertAndResolveAccountConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := UpsertAccountConfig(platformCtx, "fsbot", AccountConfig{
		AppID:     " cli_xxx ",
		AppSecret: " secret ",
	}); err != nil {
		t.Fatalf("UpsertAccountConfig returned error: %v", err)
	}

	account, ok, err := ResolveAccountConfig(platformCtx, "fsbot")
	if err != nil {
		t.Fatalf("ResolveAccountConfig returned error: %v", err)
	}
	if !ok {
		t.Fatal("ResolveAccountConfig did not find account")
	}
	if account.AppID != "cli_xxx" || account.AppSecret != "secret" || account.BaseURL != DefaultBaseURL {
		t.Fatalf("account = %#v", account)
	}
}

func TestDeleteAccountConfigRemovesOnlyNamedAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}
	if err := platformCtx.SetPlatformConfig(store.PlatformFeishu, Config{
		Accounts: map[string]AccountConfig{
			"fsbot": {AppID: "cli_xxx", AppSecret: "secret"},
			"other": {AppID: "cli_other", AppSecret: "other-secret"},
		},
		Events: []EventConfig{{Name: "p2p_chat_create", Version: "1.0", Run: ShellRun{"echo hello"}}},
	}); err != nil {
		t.Fatalf("SetPlatformConfig returned error: %v", err)
	}

	if err := DeleteAccountConfig(platformCtx, "missing"); err != nil {
		t.Fatalf("DeleteAccountConfig missing returned error: %v", err)
	}
	before, err := LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(before.Accounts) != 2 {
		t.Fatalf("accounts after missing delete = %#v, want two accounts", before.Accounts)
	}

	if err := DeleteAccountConfig(platformCtx, "fsbot"); err != nil {
		t.Fatalf("DeleteAccountConfig returned error: %v", err)
	}
	after, err := LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if _, ok := after.Accounts["fsbot"]; ok {
		t.Fatalf("fsbot account config was not deleted: %#v", after.Accounts)
	}
	if other, ok := after.Accounts["other"]; !ok || other.AppID != "cli_other" {
		t.Fatalf("other account config = %#v ok=%v", other, ok)
	}
	if len(after.Events) != 1 || after.Events[0].Name != "p2p_chat_create" {
		t.Fatalf("events = %#v, want preserved event", after.Events)
	}
}

func TestDeleteAccountConfigMissingPlatformConfigIsNoop(t *testing.T) {
	cfg := config.DefaultConfig()
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}

	if err := DeleteAccountConfig(platformCtx, "missing"); err != nil {
		t.Fatalf("DeleteAccountConfig returned error: %v", err)
	}
	if _, ok := cfg.Platforms[store.PlatformFeishu]; ok {
		t.Fatalf("feishu platform config was created by noop delete: %#v", cfg.Platforms[store.PlatformFeishu])
	}
}

func TestLoadConfigParsesEventRuns(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(`accounts:
  fsbot:
    app_id: cli_xxx
    app_secret: secret
events:
  - name: p2p_chat_create
    version: " 1.0 "
    run: echo hello
  - name: p2p_chat_create
    version: "1.0"
    run:
      - echo first
      - echo second
`), &node); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Platforms = map[string]yaml.Node{}
	cfg.Platforms[store.PlatformFeishu] = *node.Content[0]
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}

	feishuConfig, err := LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if len(feishuConfig.Events) != 2 {
		t.Fatalf("events = %#v, want two events", feishuConfig.Events)
	}
	if feishuConfig.Events[0].Version != "1.0" || feishuConfig.Events[1].Version != "1.0" {
		t.Fatalf("event versions = %#v, want trimmed 1.0", feishuConfig.Events)
	}
	if got := []string(feishuConfig.Events[0].Run); len(got) != 1 || got[0] != "echo hello" {
		t.Fatalf("first run = %#v, want scalar command", got)
	}
	if got := []string(feishuConfig.Events[1].Run); len(got) != 2 || got[0] != "echo first" || got[1] != "echo second" {
		t.Fatalf("second run = %#v, want sequence commands", got)
	}
}

func TestLoadConfigParsesSharedToolsConfig(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(`tools:
  max_results: 3
  max_chars: 2048
  chat_history:
    enabled: true
  docs:
    enabled: true
    allow_write: true
  litellm:
    enabled: true
    base_url: " https://litellm.example/ "
    api_key: " sk-admin "
    bitable:
      app_token: " base_token "
      table_id: " tbl_token "
`), &node); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Platforms = map[string]yaml.Node{}
	cfg.Platforms[store.PlatformFeishu] = *node.Content[0]
	platformCtx, err := core.NewPlatformContext(store.PlatformFeishu, &cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewPlatformContext returned error: %v", err)
	}

	feishuConfig, err := LoadConfig(platformCtx)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	tools := feishuConfig.Tools
	if tools.MaxResults != 3 || tools.MaxChars != 2048 {
		t.Fatalf("tools limits = max_results:%d max_chars:%d, want 3/2048", tools.MaxResults, tools.MaxChars)
	}
	if !tools.ChatHistory.Enabled {
		t.Fatalf("chat history config = %#v, want enabled", tools.ChatHistory)
	}
	if !tools.Docs.Enabled || !tools.Docs.AllowWrite {
		t.Fatalf("docs config = %#v, want enabled write tools", tools.Docs)
	}
	if !tools.LiteLLM.Enabled || tools.LiteLLM.BaseURL != "https://litellm.example" || tools.LiteLLM.APIKey != "sk-admin" {
		t.Fatalf("litellm config = %#v, want normalized base/api", tools.LiteLLM)
	}
	if tools.LiteLLM.UserRole != feishutools.DefaultLiteLLMUserRole {
		t.Fatalf("litellm user_role = %q, want default", tools.LiteLLM.UserRole)
	}
	if tools.LiteLLM.Bitable.AppToken != "base_token" || tools.LiteLLM.Bitable.TableID != "tbl_token" {
		t.Fatalf("litellm bitable = %#v, want normalized ids", tools.LiteLLM.Bitable)
	}
	if tools.LiteLLM.Bitable.EmailField != feishutools.DefaultLiteLLMEmailField || tools.LiteLLM.Bitable.ReasonField != feishutools.DefaultLiteLLMReasonField || tools.LiteLLM.Bitable.OwnerField != feishutools.DefaultLiteLLMOwnerField {
		t.Fatalf("litellm bitable fields = %#v, want defaults", tools.LiteLLM.Bitable)
	}
}
