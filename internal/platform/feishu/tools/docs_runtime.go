package tools

import (
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

// DocsRuntime owns the shared Docs service, its provider-facing adapters, and
// startup recovery. Keeping these dependencies together prevents callers from
// rediscovering the service by inspecting concrete tool adapter types.
type DocsRuntime struct {
	service *docsService
	tools   []tooltypes.Tool
}

// NewDocsRuntime creates the Feishu Docs runtime for one account. An
// unavailable or disabled configuration returns an inert runtime whose Tools
// and recovery methods are safe no-ops, matching the previous fail-closed tool
// registration behavior.
func NewDocsRuntime(client *lark.Client, st *store.Store, accountID string, cfg Config, approvals OperationApprovalService, resourceAccess ResourceAccessGuard, appendCipher *DocxAppendEnvelopeCipher, appendExecutionOwner string) *DocsRuntime {
	runtime := &DocsRuntime{}
	cfg = NormalizeConfig(cfg)
	accountID = strings.TrimSpace(accountID)
	appendExecutionOwner = strings.TrimSpace(appendExecutionOwner)
	if client == nil || st == nil || st.PlatformID() != store.PlatformFeishu || accountID == "" || !cfg.Docs.Enabled || resourceAccess == nil {
		return runtime
	}
	service := &docsService{
		client:                client,
		store:                 st,
		accountID:             accountID,
		cfg:                   cfg,
		approvals:             approvals,
		resourceAccess:        resourceAccess,
		appendCipher:          appendCipher,
		appendExecutionOwner:  appendExecutionOwner,
		remoteReconcileDelays: copyRemoteCreateReconciliationDelays(),
		now:                   time.Now,
	}
	runtime.service = service
	runtime.tools = []tooltypes.Tool{
		docsTool{name: searchToolName, spec: docsSearchSpec(), service: service},
		docsTool{name: readToolName, spec: docsReadSpec(), service: service},
	}
	if cfg.Docs.AllowWrite && approvals != nil && appendCipher != nil && appendExecutionOwner != "" {
		runtime.tools = append(runtime.tools, docsTool{name: createToolName, spec: docsCreateSpec(), service: service})
		runtime.tools = append(runtime.tools, docsTool{name: appendToolName, spec: docsAppendSpec(), service: service})
	}
	return runtime
}

// Tools returns an independent view of the adapters registered for this
// runtime. Mutating the returned slice cannot change runtime recovery state.
func (r *DocsRuntime) Tools() []tooltypes.Tool {
	if r == nil || len(r.tools) == 0 {
		return nil
	}
	return append([]tooltypes.Tool(nil), r.tools...)
}
