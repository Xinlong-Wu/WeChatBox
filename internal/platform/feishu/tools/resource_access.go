package tools

import (
	"context"
	"encoding/json"
	"errors"
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

	ResourceAccessStatusGranted         = "granted"
	ResourceAccessStatusPending         = "pending"
	ResourceAccessStatusDenied          = "denied"
	ResourceAccessStatusUnsupported     = "unsupported"
	ResourceAuthorizationRequiredStatus = "resource_authorization_required"

	ResourceAccessSourceBotOwner      = "bot_owner"
	ResourceAccessSourceExistingGrant = "existing_grant"
	ResourceAccessSourceNewlyGranted  = "newly_granted"

	ResourceAccessGrantModeBotOwner = "bot_owner"
	ResourceAccessGrantModeOnce     = "once"
	ResourceAccessGrantModeAll      = "all"

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

// ResourceAccessResult is returned by the public resource authorization tool.
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

// MarshalJSON emits an RFC3339 expiry only for pending authorization requests.
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

// ResourceAccessRequirement describes one side-effect-free protected-tool check.
type ResourceAccessRequirement struct {
	ResourceType  string
	ResourceToken string
	Permission    string
}

// AuthorizedResource is the trusted capability returned by the centralized
// resource guard. Protected services consume this value instead of treating a
// model-provided token as proof of authorization.
type AuthorizedResource struct {
	AccountID             string
	ActorOpenID           string
	ActorUserID           string
	ChatID                string
	ResourceType          string
	ResourceToken         string
	EffectivePermission   string
	GrantMode             string
	ExpiresAt             time.Time
	CapabilitySubjectType string
	CapabilitySubjectID   string
	Source                string
}

// ResourceAuthorizationRequiredError tells the model which exact resource grant
// must be requested before retrying the protected tool.
type ResourceAuthorizationRequiredError struct {
	Status        string `json:"status"`
	RequiredTool  string `json:"required_tool"`
	ResourceType  string `json:"resource_type"`
	ResourceToken string `json:"resource_token"`
	Permission    string `json:"permission"`
	Message       string `json:"message"`
}

func (e *ResourceAuthorizationRequiredError) Error() string {
	if e == nil {
		return "feishu resource authorization is required"
	}
	return e.Message
}

// ResourceAuthorizationRequiredContent returns the structured tool-error
// payload even when callers wrapped the underlying authorization error.
func ResourceAuthorizationRequiredContent(err error) (string, bool) {
	var required *ResourceAuthorizationRequiredError
	if !errors.As(err, &required) || required == nil {
		return "", false
	}
	data, marshalErr := json.Marshal(required)
	if marshalErr != nil {
		return required.Error(), true
	}
	return string(data), true
}

// NewResourceAuthorizationRequiredError builds the stable structured error
// returned by protected Feishu tools. An empty message uses the standard retry
// instruction for the required permission.
func NewResourceAuthorizationRequiredError(requirement ResourceAccessRequirement, message string) error {
	permissionLabel := "读取"
	if requirement.Permission == ResourcePermissionWrite {
		permissionLabel = "写入"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = fmt.Sprintf("请先调用 %s 获取该资源的%s授权，然后重试原工具", ResourceAccessToolName, permissionLabel)
	}
	return &ResourceAuthorizationRequiredError{
		Status:        ResourceAuthorizationRequiredStatus,
		RequiredTool:  ResourceAccessToolName,
		ResourceType:  requirement.ResourceType,
		ResourceToken: requirement.ResourceToken,
		Permission:    requirement.Permission,
		Message:       message,
	}
}

// ResourceAccessGuard is the only authorization entry point for protected
// Feishu resource operations.
type ResourceAccessGuard interface {
	Require(context.Context, ResourceAccessRequirement) (AuthorizedResource, error)
}

// ResourceAccessController owns both the explicit authorization workflow and
// the side-effect-free guard used by protected tools.
type ResourceAccessController interface {
	ResourceAccessGuard
	RequestAccess(context.Context, ResourceAccessRequest) (ResourceAccessResult, error)
}

func requireResourceAccess(ctx context.Context, guard ResourceAccessGuard, accountID string, requirement ResourceAccessRequirement) (AuthorizedResource, error) {
	accountID = strings.TrimSpace(accountID)
	requirement.ResourceType = NormalizeResourceType(requirement.ResourceType)
	requirement.ResourceToken = strings.TrimSpace(requirement.ResourceToken)
	requirement.Permission = strings.ToLower(strings.TrimSpace(requirement.Permission))
	if guard == nil {
		return AuthorizedResource{}, fmt.Errorf("feishu resource access guard is unavailable")
	}
	if accountID == "" || !SupportedResourceType(requirement.ResourceType) || requirement.ResourceToken == "" ||
		(requirement.Permission != ResourcePermissionRead && requirement.Permission != ResourcePermissionWrite) {
		return AuthorizedResource{}, fmt.Errorf("valid feishu account, resource type, token, and read/write permission are required")
	}
	authorized, err := guard.Require(ctx, requirement)
	if err != nil {
		return AuthorizedResource{}, err
	}
	authorized.AccountID = strings.TrimSpace(authorized.AccountID)
	authorized.ActorOpenID = strings.TrimSpace(authorized.ActorOpenID)
	authorized.ActorUserID = strings.TrimSpace(authorized.ActorUserID)
	authorized.ChatID = strings.TrimSpace(authorized.ChatID)
	authorized.ResourceType = NormalizeResourceType(authorized.ResourceType)
	authorized.ResourceToken = strings.TrimSpace(authorized.ResourceToken)
	authorized.EffectivePermission = strings.ToLower(strings.TrimSpace(authorized.EffectivePermission))
	authorized.GrantMode = strings.ToLower(strings.TrimSpace(authorized.GrantMode))
	authorized.CapabilitySubjectType = strings.TrimSpace(authorized.CapabilitySubjectType)
	authorized.CapabilitySubjectID = strings.TrimSpace(authorized.CapabilitySubjectID)
	authorized.Source = strings.TrimSpace(authorized.Source)
	actor, actorOK := ActorFromContext(ctx)
	chat, chatOK := ChatContextFromContext(ctx)
	if !actorOK || (actor.OpenID == "" && actor.UserID == "") || !chatOK || chat.ChatID == "" {
		return AuthorizedResource{}, fmt.Errorf("trusted feishu actor and chat are required for resource access")
	}
	actorMatches := authorized.ActorOpenID != "" || authorized.ActorUserID != ""
	if authorized.ActorOpenID != "" && authorized.ActorOpenID != strings.TrimSpace(actor.OpenID) {
		actorMatches = false
	}
	if authorized.ActorUserID != "" && authorized.ActorUserID != strings.TrimSpace(actor.UserID) {
		actorMatches = false
	}
	grantValid := false
	switch authorized.GrantMode {
	case ResourceAccessGrantModeBotOwner:
		grantValid = authorized.Source == ResourceAccessSourceBotOwner && authorized.ExpiresAt.IsZero()
	case ResourceAccessGrantModeOnce:
		grantValid = (authorized.Source == ResourceAccessSourceExistingGrant || authorized.Source == ResourceAccessSourceNewlyGranted) &&
			!authorized.ExpiresAt.IsZero() && time.Now().UTC().Before(authorized.ExpiresAt)
	case ResourceAccessGrantModeAll:
		grantValid = (authorized.Source == ResourceAccessSourceExistingGrant || authorized.Source == ResourceAccessSourceNewlyGranted) && authorized.ExpiresAt.IsZero()
	}
	if authorized.AccountID != accountID || !actorMatches || authorized.ChatID != chat.ChatID ||
		authorized.ResourceType != requirement.ResourceType || authorized.ResourceToken != requirement.ResourceToken ||
		!resourcePermissionSatisfies(authorized.EffectivePermission, requirement.Permission) ||
		!grantValid || authorized.CapabilitySubjectType == "" || authorized.CapabilitySubjectID == "" {
		return AuthorizedResource{}, fmt.Errorf("feishu resource access guard returned mismatched or incomplete authorization")
	}
	return authorized, nil
}

func resourcePermissionSatisfies(granted, required string) bool {
	granted = strings.ToLower(strings.TrimSpace(granted))
	required = strings.ToLower(strings.TrimSpace(required))
	return granted == required || (granted == ResourcePermissionWrite && required == ResourcePermissionRead)
}

func authorizedResourcePermits(resource AuthorizedResource, resourceType, resourceToken, permission string) bool {
	return NormalizeResourceType(resource.ResourceType) == NormalizeResourceType(resourceType) &&
		strings.TrimSpace(resource.ResourceToken) == strings.TrimSpace(resourceToken) &&
		resourcePermissionSatisfies(resource.EffectivePermission, permission)
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
		Description: "Verify or request read/write access to one exact Feishu file or folder for the trusted current user and chat. Call this when a protected Feishu tool returns status=resource_authorization_required, using that error's resource_type, resource_token, and permission. Bot-owned resources and existing valid grants return granted immediately. Otherwise the requester chooses either a temporary 10–60 minute grant or a permanent grant in a card; once_duration_minutes is the model's suggested temporary window and is shown to the user. write also satisfies read, while read never satisfies write. If Feishu-side permission is missing, LingoBridge reuses the requester's encrypted OAuth credential or updates the same card with Feishu's official OAuth handoff. Use resource_token=bot_root for the Bot root or chat_default_folder for the current chat's default Bot folder.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"resource_type":{"type":"string","enum":["folder","doc","docx","sheet","file","wiki","bitable","mindnote","minutes","slides"]},"resource_token":{"type":"string","description":"Exact Feishu resource token, or bot_root/chat_default_folder for a trusted Bot folder alias."},"resource_url":{"type":"string","description":"Optional Feishu resource URL used in the authorization card and post-OAuth redirect."},"permission":{"type":"string","enum":["read","write"]},"once_duration_minutes":{"type":"integer","minimum":10,"maximum":60,"description":"Suggested temporary authorization window shown on the card; the user can instead choose permanent authorization."},"reason":{"type":"string","maxLength":500,"description":"Short user-visible reason for requesting access."}},"required":["resource_type","resource_token","permission","once_duration_minutes"],"additionalProperties":false}`),
	}
}
