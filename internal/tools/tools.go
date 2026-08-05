package tools

import (
	"context"
	"encoding/json"
	"time"
)

// Spec describes one client-side function exposed to an LLM provider.
type Spec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Call is a provider-neutral request to run a client-side tool.
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Result is the result returned to the provider for one tool call.
type Result struct {
	CallID  string
	Name    string
	Content string
	IsError bool
	// PendingWorkflowID is runtime-only coordination metadata. Providers and
	// models receive Content, never this trusted workflow identifier.
	PendingWorkflowID string
}

// Options controls generic tool loop execution limits.
type Options struct {
	MaxCalls    int
	Timeout     time.Duration
	ResultLimit int
}

// Scope identifies the bot/account currently resolving tools.
type Scope struct {
	Platform    string
	AccountID   string
	AccountName string
}

// Selection is the provider-resolved tool set and shared execution options.
type Selection struct {
	Tools   []Tool
	Options Options
}

// Tool is a function that can be exposed to a tool-capable LLM.
type Tool interface {
	Spec() Spec
	Execute(ctx context.Context, call Call) Result
}

// Provider resolves tools and generic execution options for one scope.
type Provider interface {
	Resolve(scope Scope) Selection
}
