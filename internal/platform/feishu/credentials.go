package feishu

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"lingobridge/internal/core"
	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	DefaultBaseURL      = "https://open.feishu.cn"
	DefaultOAuthBaseURL = "https://accounts.feishu.cn"
	TextChunkLimit      = 25 * 1024
)

type Credentials struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// Config holds Feishu platform-private configuration.
type Config struct {
	Accounts map[string]AccountConfig `yaml:"accounts"`
	Events   []EventConfig            `yaml:"events,omitempty"`
	Tools    feishutools.Config       `yaml:"tools,omitempty"`
}

// AccountConfig holds one Feishu self-built app account config.
type AccountConfig struct {
	AppID                      string `yaml:"app_id"`
	AppSecret                  string `yaml:"app_secret"`
	BaseURL                    string `yaml:"base_url"`
	OAuthBaseURL               string `yaml:"oauth_base_url,omitempty"`
	OAuthCallbackURL           string `yaml:"oauth_callback_url,omitempty"`
	OAuthCallbackListenAddress string `yaml:"oauth_callback_listen_address,omitempty"`
}

// EventConfig holds one configured Feishu event shell hook.
type EventConfig struct {
	Name    string   `yaml:"name"`
	Version string   `yaml:"version"`
	Run     ShellRun `yaml:"run"`
}

// ShellRun is one shell script or a sequence of shell scripts.
type ShellRun []string

func (r *ShellRun) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(value.Value) == "" {
			*r = nil
			return nil
		}
		*r = ShellRun{value.Value}
		return nil
	case yaml.SequenceNode:
		var commands []string
		for _, item := range value.Content {
			var command string
			if err := item.Decode(&command); err != nil {
				return fmt.Errorf("decode run command: %w", err)
			}
			if strings.TrimSpace(command) != "" {
				commands = append(commands, command)
			}
		}
		*r = ShellRun(commands)
		return nil
	default:
		return fmt.Errorf("run must be a string or a list of strings")
	}
}

func NewAccount(name, appID, appSecret, baseURL string) (store.Account, error) {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if appID == "" {
		return store.Account{}, fmt.Errorf("feishu app_id is required")
	}
	if appSecret == "" {
		return store.Account{}, fmt.Errorf("feishu app_secret is required")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	return store.Account{
		ID:              "feishu:" + appID,
		Name:            name,
		Platform:        store.PlatformFeishu,
		BaseURL:         baseURL,
		UserID:          appID,
		CredentialsJSON: "{}",
		Enabled:         true,
	}, nil
}

// LoadConfig decodes the Feishu platform config through the core config API.
func LoadConfig(api core.PlatformConfigAPI) (Config, error) {
	var cfg Config
	if err := api.GetPlatformConfig(store.PlatformFeishu, &cfg); err != nil {
		if errors.Is(err, core.ErrPlatformConfigNotFound) {
			cfg.ApplyDefaults()
			return cfg, nil
		}
		return Config{}, err
	}
	cfg.ApplyDefaults()
	return cfg, nil
}

// UpsertAccountConfig writes one Feishu account config through the core config API.
func UpsertAccountConfig(api core.PlatformConfigAPI, name string, account AccountConfig) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("feishu account name is required")
	}
	account = normalizeAccountConfig(account)
	if account.AppID == "" {
		return fmt.Errorf("platforms.feishu.accounts.%s.app_id is required", name)
	}
	if account.AppSecret == "" {
		return fmt.Errorf("platforms.feishu.accounts.%s.app_secret is required", name)
	}
	cfg, err := LoadConfig(api)
	if err != nil {
		return err
	}
	if cfg.Accounts == nil {
		cfg.Accounts = map[string]AccountConfig{}
	}
	cfg.Accounts[name] = account
	return api.SetPlatformConfig(store.PlatformFeishu, cfg)
}

// DeleteAccountConfig removes one Feishu account config if it exists.
func DeleteAccountConfig(api core.PlatformConfigAPI, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("feishu account name is required")
	}
	var cfg Config
	if err := api.GetPlatformConfig(store.PlatformFeishu, &cfg); err != nil {
		if errors.Is(err, core.ErrPlatformConfigNotFound) {
			return nil
		}
		return err
	}
	if cfg.Accounts == nil {
		return nil
	}
	if _, ok := cfg.Accounts[name]; !ok {
		return nil
	}
	delete(cfg.Accounts, name)
	cfg.ApplyDefaults()
	return api.SetPlatformConfig(store.PlatformFeishu, cfg)
}

// ResolveAccountConfig returns a validated Feishu account config by account name.
func ResolveAccountConfig(api core.PlatformConfigAPI, name string) (AccountConfig, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return AccountConfig{}, false, nil
	}
	cfg, err := LoadConfig(api)
	if err != nil {
		return AccountConfig{}, false, err
	}
	account, ok := cfg.Accounts[name]
	if !ok {
		return AccountConfig{}, false, nil
	}
	account = normalizeAccountConfig(account)
	if account.AppID == "" {
		return account, true, fmt.Errorf("platforms.feishu.accounts.%s.app_id is required", name)
	}
	if account.AppSecret == "" {
		return account, true, fmt.Errorf("platforms.feishu.accounts.%s.app_secret is required", name)
	}
	return account, true, nil
}

// CredentialsFromConfig returns SDK credentials from a Feishu account config.
func CredentialsFromConfig(account AccountConfig) Credentials {
	return Credentials{
		AppID:     strings.TrimSpace(account.AppID),
		AppSecret: strings.TrimSpace(account.AppSecret),
	}
}

// ApplyDefaults fills optional Feishu platform config fields.
func (c *Config) ApplyDefaults() {
	if c.Accounts == nil {
		c.Accounts = map[string]AccountConfig{}
	}
	for name, account := range c.Accounts {
		c.Accounts[name] = normalizeAccountConfig(account)
	}
	for i, event := range c.Events {
		event.Name = strings.TrimSpace(event.Name)
		event.Version = strings.TrimSpace(event.Version)
		event.Run = normalizeShellRun(event.Run)
		c.Events[i] = event
	}
	c.Tools = feishutools.NormalizeConfig(c.Tools)
}

func normalizeAccountConfig(account AccountConfig) AccountConfig {
	account.AppID = strings.TrimSpace(account.AppID)
	account.AppSecret = strings.TrimSpace(account.AppSecret)
	account.BaseURL = strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	account.OAuthBaseURL = strings.TrimRight(strings.TrimSpace(account.OAuthBaseURL), "/")
	account.OAuthCallbackURL = strings.TrimSpace(account.OAuthCallbackURL)
	account.OAuthCallbackListenAddress = strings.TrimSpace(account.OAuthCallbackListenAddress)
	if account.BaseURL == "" {
		account.BaseURL = DefaultBaseURL
	}
	if account.OAuthBaseURL == "" && account.BaseURL == DefaultBaseURL {
		account.OAuthBaseURL = DefaultOAuthBaseURL
	}
	return account
}

func normalizeShellRun(run ShellRun) ShellRun {
	normalized := ShellRun{}
	for _, command := range run {
		if strings.TrimSpace(command) != "" {
			normalized = append(normalized, command)
		}
	}
	return normalized
}
