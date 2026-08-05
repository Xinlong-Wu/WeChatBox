package platform

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"lingobridge/internal/commands"
	"lingobridge/internal/config"
	"lingobridge/internal/core"
	"lingobridge/internal/logging"
	"lingobridge/internal/session"
	"lingobridge/internal/store"
)

type AccountNewOptions struct {
	Name   string
	Values any
}

type AccountNewIO struct {
	In  io.Reader
	Out io.Writer
}

type AccountNewContext struct {
	Platform *core.PlatformContext
}

type AccountListContext struct {
	Platform *core.PlatformContext
}

type AccountDeleteContext struct {
	Platform *core.PlatformContext
	Account  store.Account
}

type RuntimeContext struct {
	Store     *store.Store
	Sessions  *session.Manager
	Platform  *core.PlatformContext
	Config    config.Config
	LLMConfig config.LLMConfig
	Account   store.Account
	LogLevel  logging.Level
}

type Definition struct {
	ID                    string
	Aliases               []string
	AccountNewUsage       string
	ParseAccountNewFlags  func(args []string, io AccountNewIO) (AccountNewOptions, error)
	ListAccounts          func(ctx AccountListContext) ([]store.Account, error)
	CreateOrUpdateAccount func(ctx AccountNewContext, opts AccountNewOptions) error
	DeleteAccount         func(ctx AccountDeleteContext) error
	NewRuntimePlatform    func(ctx RuntimeContext) (core.Platform, error)
	CommandPolicy         commands.Policy
	TextChunkLimit        int
	EnableTextStreaming   bool
}

func DefaultAccountNewIO() AccountNewIO {
	return AccountNewIO{In: os.Stdin, Out: os.Stdout}
}

type Registry struct {
	definitions map[string]Definition
	aliases     map[string]string
}

type policyPlatform struct {
	inner  core.Platform
	policy commands.Policy
}

type policyHandler struct {
	next   core.Handler
	policy commands.Policy
}

type policyWorkflowHandler struct {
	policyHandler
	resumer core.WorkflowResumer
}

func (p policyPlatform) Run(ctx context.Context, handler core.Handler) error {
	return p.inner.Run(ctx, wrapPolicyHandler(handler, p.policy))
}

func (h policyHandler) Handle(ctx context.Context, msg core.InboundMessage, sender core.Sender) error {
	msg.CommandPolicy = h.policy
	return h.next.Handle(ctx, msg, sender)
}

func (h policyWorkflowHandler) ResumeWorkflow(ctx context.Context, request core.WorkflowResumeRequest, sender core.Sender) error {
	return h.resumer.ResumeWorkflow(ctx, request, sender)
}

func wrapPolicyHandler(next core.Handler, policy commands.Policy) core.Handler {
	wrapped := policyHandler{next: next, policy: policy}
	if resumer, ok := next.(core.WorkflowResumer); ok {
		return policyWorkflowHandler{policyHandler: wrapped, resumer: resumer}
	}
	return wrapped
}

var _ core.Handler = policyHandler{}
var _ core.Handler = policyWorkflowHandler{}
var _ core.WorkflowResumer = policyWorkflowHandler{}

func NewRegistry() *Registry {
	return &Registry{
		definitions: map[string]Definition{},
		aliases:     map[string]string{},
	}
}

func (r *Registry) Register(def Definition) error {
	id := normalizePlatformName(def.ID)
	if id == "" {
		return fmt.Errorf("platform id is required")
	}
	if _, ok := r.definitions[id]; ok {
		return fmt.Errorf("platform %q is already registered", id)
	}
	def.ID = id
	if def.ParseAccountNewFlags == nil {
		return fmt.Errorf("platform %q missing account new parser", id)
	}
	if def.ListAccounts == nil {
		return fmt.Errorf("platform %q missing account lister", id)
	}
	if def.CreateOrUpdateAccount == nil {
		return fmt.Errorf("platform %q missing account creator", id)
	}
	if def.DeleteAccount == nil {
		return fmt.Errorf("platform %q missing account deleter", id)
	}
	if def.NewRuntimePlatform == nil {
		return fmt.Errorf("platform %q missing runtime factory", id)
	}

	r.definitions[id] = def
	for _, name := range append([]string{id}, def.Aliases...) {
		alias := normalizePlatformName(name)
		if alias == "" {
			continue
		}
		if existing, ok := r.aliases[alias]; ok {
			return fmt.Errorf("platform alias %q already maps to %q", alias, existing)
		}
		r.aliases[alias] = id
	}
	return nil
}

func (r *Registry) Lookup(name string) (Definition, bool) {
	id, ok := r.aliases[normalizePlatformName(name)]
	if !ok {
		return Definition{}, false
	}
	def, ok := r.definitions[id]
	return def, ok
}

func (r *Registry) LookupAccountPlatform(platform string) (Definition, bool) {
	platform = normalizePlatformName(platform)
	return r.Lookup(platform)
}

func (r *Registry) PlatformNames() []string {
	names := make([]string, 0, len(r.definitions))
	for id := range r.definitions {
		names = append(names, id)
	}
	sort.Strings(names)
	return names
}

func (d Definition) RuntimePlatform(ctx RuntimeContext) (core.Platform, error) {
	p, err := d.NewRuntimePlatform(ctx)
	if err != nil {
		return nil, err
	}
	return policyPlatform{inner: p, policy: d.CommandPolicy}, nil
}

func normalizePlatformName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
