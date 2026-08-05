package tools

import "strings"

// Config holds optional LLM tool plugins exposed by the Feishu platform.
type Config struct {
	MaxResults  int                `yaml:"max_results,omitempty"`
	MaxChars    int                `yaml:"max_chars,omitempty"`
	ChatHistory ChatHistoryConfig  `yaml:"chat_history,omitempty"`
	Docs        DocsToolsConfig    `yaml:"docs,omitempty"`
	LiteLLM     LiteLLMToolsConfig `yaml:"litellm,omitempty"`
}

// ChatHistoryConfig controls the current-chat Feishu message history tool.
type ChatHistoryConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// DocsToolsConfig controls Feishu document tools exposed to tool-capable models.
type DocsToolsConfig struct {
	Enabled    bool `yaml:"enabled,omitempty"`
	AllowWrite bool `yaml:"allow_write,omitempty"`
}

// LiteLLMToolsConfig controls the Feishu-triggered LiteLLM account invitation tool.
type LiteLLMToolsConfig struct {
	Enabled  bool                 `yaml:"enabled,omitempty"`
	BaseURL  string               `yaml:"base_url,omitempty"`
	APIKey   string               `yaml:"api_key,omitempty"`
	UserRole string               `yaml:"user_role,omitempty"`
	Bitable  LiteLLMBitableConfig `yaml:"bitable,omitempty"`
}

// LiteLLMBitableConfig identifies the Bitable table and field names used as the audit log.
type LiteLLMBitableConfig struct {
	AppToken    string `yaml:"app_token,omitempty"`
	TableID     string `yaml:"table_id,omitempty"`
	EmailField  string `yaml:"email_field,omitempty"`
	ReasonField string `yaml:"reason_field,omitempty"`
	OwnerField  string `yaml:"owner_field,omitempty"`
}

const (
	DefaultMaxResults         = 5
	DefaultMaxChars           = 12000
	DefaultLiteLLMEmailField  = "邮箱"
	DefaultLiteLLMReasonField = "申请原因"
	DefaultLiteLLMOwnerField  = "所有者"
	DefaultLiteLLMUserRole    = "internal_user"
)

// NormalizeConfig fills defaults and trims user-provided Feishu tools config.
func NormalizeConfig(cfg Config) Config {
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = DefaultMaxResults
	}
	if cfg.MaxChars <= 0 {
		cfg.MaxChars = DefaultMaxChars
	}
	cfg.LiteLLM = normalizeLiteLLMConfig(cfg.LiteLLM)
	return cfg
}

func normalizeLiteLLMConfig(cfg LiteLLMToolsConfig) LiteLLMToolsConfig {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.UserRole = strings.TrimSpace(cfg.UserRole)
	cfg.Bitable.AppToken = strings.TrimSpace(cfg.Bitable.AppToken)
	cfg.Bitable.TableID = strings.TrimSpace(cfg.Bitable.TableID)
	cfg.Bitable.EmailField = strings.TrimSpace(cfg.Bitable.EmailField)
	cfg.Bitable.ReasonField = strings.TrimSpace(cfg.Bitable.ReasonField)
	cfg.Bitable.OwnerField = strings.TrimSpace(cfg.Bitable.OwnerField)
	if !liteLLMConfigPresent(cfg) {
		return cfg
	}
	if cfg.UserRole == "" {
		cfg.UserRole = DefaultLiteLLMUserRole
	}
	if cfg.Bitable.EmailField == "" {
		cfg.Bitable.EmailField = DefaultLiteLLMEmailField
	}
	if cfg.Bitable.ReasonField == "" {
		cfg.Bitable.ReasonField = DefaultLiteLLMReasonField
	}
	if cfg.Bitable.OwnerField == "" {
		cfg.Bitable.OwnerField = DefaultLiteLLMOwnerField
	}
	return cfg
}

func liteLLMConfigPresent(cfg LiteLLMToolsConfig) bool {
	return cfg.Enabled ||
		cfg.BaseURL != "" ||
		cfg.APIKey != "" ||
		cfg.UserRole != "" ||
		cfg.Bitable.AppToken != "" ||
		cfg.Bitable.TableID != "" ||
		cfg.Bitable.EmailField != "" ||
		cfg.Bitable.ReasonField != "" ||
		cfg.Bitable.OwnerField != ""
}
