package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tooltypes "lingobridge/internal/tools"
)

const (
	ResourceAccessToolName = "feishu_docs_request_access"

	BotRootResourceAlias           = "bot_root"
	ChatDefaultFolderResourceAlias = "chat_default_folder"

	ResourcePermissionRead  = "read"
	ResourcePermissionWrite = "write"

	ResourceAccessStatusGranted     = "granted"
	ResourceAccessStatusPending     = "pending"
	ResourceAccessStatusDenied      = "denied"
	ResourceAccessStatusUnsupported = "unsupported"

	ResourceAccessSourceBotOwner      = "bot_owner"
	ResourceAccessSourceExistingGrant = "existing_grant"
	ResourceAccessSourceNewlyGranted  = "newly_granted"

	ResourceAccessMinOnceDurationMinutes = 10
	ResourceAccessMaxOnceDurationMinutes = 60

	maxResourceAccessReasonRunes = 500
)

// ResourceAccessRequest asks the platform runtime to verify or obtain one
// read/write permission for the trusted current Feishu chat.
type ResourceAccessRequest struct {
	ResourceType        string `json:"resource_type"`
	ResourceToken       string `json:"resource_token"`
	ResourceURL         string `json:"resource_url,omitempty"`
	Permission          string `json:"permission"`
	OnceDurationMinutes int    `json:"once_duration_minutes"`
	Reason              string `json:"reason,omitempty"`
}

// ResourceAccessResult is returned by both the public tool and create-time validation.
type ResourceAccessResult struct {
	RequestID     string    `json:"request_id"`
	Status        string    `json:"status"`
	Permission    string    `json:"permission"`
	Source        string    `json:"source,omitempty"`
	ResourceType  string    `json:"resource_type"`
	ResourceToken string    `json:"resource_token"`
	ResourceURL   string    `json:"resource_url,omitempty"`
	ExpiresAt     time.Time `json:"-"`
	Message       string    `json:"message,omitempty"`
}

// MarshalJSON emits an RFC3339 expiry only for pending OAuth requests.
func (r ResourceAccessResult) MarshalJSON() ([]byte, error) {
	type resultAlias ResourceAccessResult
	value := struct {
		resultAlias
		ExpiresAt string `json:"expires_at,omitempty"`
	}{resultAlias: resultAlias(r)}
	if !r.ExpiresAt.IsZero() {
		value.ExpiresAt = r.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(value)
}

// ResourceAccessValidation binds a create operation to one prior access request.
type ResourceAccessValidation struct {
	RequestID     string
	ResourceType  string
	ResourceToken string
	Permission    string
}

// ResourceAccessController owns resource resolution, live Feishu verification,
// OAuth/card delivery, and create-time request validation.
type ResourceAccessController interface {
	RequestAccess(context.Context, ResourceAccessRequest) (ResourceAccessResult, error)
	ValidateAccess(context.Context, ResourceAccessValidation) (ResourceAccessResult, error)
	ConsumeAccess(context.Context, ResourceAccessValidation, string) (ResourceAccessResult, error)
}

func validateGrantedResourceAccess(ctx context.Context, controller ResourceAccessController, validation ResourceAccessValidation) (ResourceAccessResult, error) {
	validation.RequestID = strings.TrimSpace(validation.RequestID)
	validation.ResourceType = NormalizeResourceType(validation.ResourceType)
	validation.ResourceToken = strings.TrimSpace(validation.ResourceToken)
	validation.Permission = strings.ToLower(strings.TrimSpace(validation.Permission))
	if controller == nil {
		return ResourceAccessResult{}, fmt.Errorf("feishu resource access workflow is unavailable")
	}
	if validation.RequestID == "" {
		return ResourceAccessResult{}, fmt.Errorf("access_request_id is required; call %s before using the resource", ResourceAccessToolName)
	}
	result, err := controller.ValidateAccess(ctx, validation)
	if err != nil {
		return ResourceAccessResult{}, err
	}
	if result.Status != ResourceAccessStatusGranted || strings.TrimSpace(result.RequestID) != validation.RequestID {
		return ResourceAccessResult{}, fmt.Errorf("access_request_id is not a granted request for this operation; call %s again", ResourceAccessToolName)
	}
	return result, nil
}

func consumeGrantedResourceAccess(ctx context.Context, controller ResourceAccessController, validation ResourceAccessValidation, consumingRequestID string) (ResourceAccessResult, error) {
	consumingRequestID = strings.TrimSpace(consumingRequestID)
	if consumingRequestID == "" {
		return ResourceAccessResult{}, fmt.Errorf("consuming workflow request_id is required")
	}
	if controller == nil {
		return ResourceAccessResult{}, fmt.Errorf("feishu resource access workflow is unavailable")
	}
	result, err := controller.ConsumeAccess(ctx, validation, consumingRequestID)
	if err != nil {
		return ResourceAccessResult{}, err
	}
	if result.Status != ResourceAccessStatusGranted || strings.TrimSpace(result.RequestID) != strings.TrimSpace(validation.RequestID) {
		return ResourceAccessResult{}, fmt.Errorf("access_request_id was not consumed for this operation; call %s again", ResourceAccessToolName)
	}
	return result, nil
}

type resourceAccessTool struct {
	controller ResourceAccessController
}

// NewDocsResourceAccessTools returns the chat-scoped resource access tool.
func NewDocsResourceAccessTools(controller ResourceAccessController, cfg Config) []tooltypes.Tool {
	cfg = NormalizeConfig(cfg)
	if controller == nil || !cfg.Docs.Enabled {
		return nil
	}
	return []tooltypes.Tool{resourceAccessTool{controller: controller}}
}

func (t resourceAccessTool) Spec() tooltypes.Spec {
	return docsResourceAccessSpec()
}

func (t resourceAccessTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	request, err := parseResourceAccessRequest(call.Arguments)
	if err != nil {
		return tooltypes.Result{CallID: call.ID, Name: ResourceAccessToolName, Content: err.Error(), IsError: true}
	}
	result, err := t.controller.RequestAccess(ctx, request)
	pendingWorkflowID := ""
	if result.Status == ResourceAccessStatusPending {
		pendingWorkflowID = result.RequestID
	}
	content := ""
	if err == nil {
		content, err = marshalToolOutput(result)
	}
	return tooltypes.Result{
		CallID:            call.ID,
		Name:              ResourceAccessToolName,
		Content:           contentOrError(content, err),
		IsError:           err != nil,
		PendingWorkflowID: pendingWorkflowID,
	}
}

func parseResourceAccessRequest(raw json.RawMessage) (ResourceAccessRequest, error) {
	var request ResourceAccessRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return ResourceAccessRequest{}, fmt.Errorf("parse arguments: %w", err)
	}
	request.ResourceType = NormalizeResourceType(request.ResourceType)
	request.ResourceToken = strings.TrimSpace(request.ResourceToken)
	request.ResourceURL = strings.TrimSpace(request.ResourceURL)
	request.Permission = strings.ToLower(strings.TrimSpace(request.Permission))
	request.Reason = strings.TrimSpace(request.Reason)
	if !SupportedResourceType(request.ResourceType) {
		return ResourceAccessRequest{}, fmt.Errorf("unsupported resource_type %q", request.ResourceType)
	}
	if request.ResourceToken == "" {
		return ResourceAccessRequest{}, fmt.Errorf("resource_token is required")
	}
	if !ResourceTokenAlias(request.ResourceToken) && !tokenPattern.MatchString(request.ResourceToken) {
		return ResourceAccessRequest{}, fmt.Errorf("invalid resource_token")
	}
	if request.Permission != ResourcePermissionRead && request.Permission != ResourcePermissionWrite {
		return ResourceAccessRequest{}, fmt.Errorf("permission must be read or write")
	}
	if request.OnceDurationMinutes < ResourceAccessMinOnceDurationMinutes || request.OnceDurationMinutes > ResourceAccessMaxOnceDurationMinutes {
		return ResourceAccessRequest{}, fmt.Errorf("once_duration_minutes must be between %d and %d", ResourceAccessMinOnceDurationMinutes, ResourceAccessMaxOnceDurationMinutes)
	}
	if utf8.RuneCountInString(request.Reason) > maxResourceAccessReasonRunes {
		return ResourceAccessRequest{}, fmt.Errorf("reason must not exceed %d characters", maxResourceAccessReasonRunes)
	}
	if ResourceTokenAlias(request.ResourceToken) && request.ResourceType != "folder" {
		return ResourceAccessRequest{}, fmt.Errorf("resource_token alias %q requires resource_type=folder", request.ResourceToken)
	}
	return request, nil
}

// NormalizeResourceType maps common document aliases to Feishu permission API types.
func NormalizeResourceType(resourceType string) string {
	resourceType = strings.ToLower(strings.TrimSpace(resourceType))
	switch resourceType {
	case "document", "docs":
		return "docx"
	default:
		return resourceType
	}
}

// SupportedResourceType reports whether the Drive permission API supports the type.
func SupportedResourceType(resourceType string) bool {
	switch NormalizeResourceType(resourceType) {
	case "folder", "doc", "docx", "sheet", "file", "wiki", "bitable", "mindnote", "minutes", "slides":
		return true
	default:
		return false
	}
}

// ResourceTokenAlias reports whether a token names a trusted runtime-resolved folder.
func ResourceTokenAlias(token string) bool {
	switch strings.TrimSpace(token) {
	case BotRootResourceAlias, ChatDefaultFolderResourceAlias:
		return true
	default:
		return false
	}
}

func docsResourceAccessSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        ResourceAccessToolName,
		Description: "Verify or request read/write access to one exact Feishu file or folder for the trusted current user and chat. Bot-owned resources and existing valid grants return granted immediately. Otherwise the requester chooses either a temporary 10–60 minute grant or a permanent grant in a card; once_duration_minutes is the model's suggested temporary window and is shown to the user. write also satisfies read, while read never satisfies write. If Feishu-side permission is missing, LingoBridge reuses the requester's encrypted OAuth credential or updates the same card with Feishu's official OAuth handoff. Use resource_token=bot_root for the Bot root or chat_default_folder for the current chat's default Bot folder.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"resource_type":{"type":"string","enum":["folder","doc","docx","sheet","file","wiki","bitable","mindnote","minutes","slides"]},"resource_token":{"type":"string","description":"Exact Feishu resource token, or bot_root/chat_default_folder for a trusted Bot folder alias."},"resource_url":{"type":"string","description":"Optional Feishu resource URL used in the authorization card and post-OAuth redirect."},"permission":{"type":"string","enum":["read","write"]},"once_duration_minutes":{"type":"integer","minimum":10,"maximum":60,"description":"Suggested temporary authorization window shown on the card; the user can instead choose permanent authorization."},"reason":{"type":"string","maxLength":500,"description":"Short user-visible reason for requesting access."}},"required":["resource_type","resource_token","permission","once_duration_minutes"],"additionalProperties":false}`),
	}
}
