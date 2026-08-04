package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tooltypes "lingobridge/internal/tools"
)

type fakeResourceAccessController struct {
	request      ResourceAccessRequest
	requirement  ResourceAccessRequirement
	requirements []ResourceAccessRequirement
	result       ResourceAccessResult
	err          error
	actor        Actor
	chat         ChatContext
}

type staticResourceAccessGuard struct {
	authorized AuthorizedResource
	err        error
}

func (g staticResourceAccessGuard) Require(context.Context, ResourceAccessRequirement) (AuthorizedResource, error) {
	return g.authorized, g.err
}

func (f *fakeResourceAccessController) RequestAccess(_ context.Context, request ResourceAccessRequest) (ResourceAccessResult, error) {
	f.request = request
	return f.result, f.err
}

func (f *fakeResourceAccessController) Require(ctx context.Context, requirement ResourceAccessRequirement) (AuthorizedResource, error) {
	f.requirement = requirement
	f.requirements = append(f.requirements, requirement)
	f.actor, _ = ActorFromContext(ctx)
	f.chat, _ = ChatContextFromContext(ctx)
	if f.err != nil {
		return AuthorizedResource{}, f.err
	}
	return AuthorizedResource{
		AccountID:             "feishu:cli_test",
		ActorOpenID:           f.actor.OpenID,
		ActorUserID:           f.actor.UserID,
		ChatID:                f.chat.ChatID,
		ResourceType:          requirement.ResourceType,
		ResourceToken:         requirement.ResourceToken,
		EffectivePermission:   requirement.Permission,
		GrantMode:             ResourceAccessGrantModeAll,
		CapabilitySubjectType: "bot",
		CapabilitySubjectID:   "ou_bot",
		Source:                ResourceAccessSourceExistingGrant,
	}, nil
}

func grantedResourceAccessController(requestID string) *fakeResourceAccessController {
	return &fakeResourceAccessController{result: ResourceAccessResult{
		RequestID: requestID,
		Status:    ResourceAccessStatusGranted,
	}}
}

func TestDocsResourceAccessToolRegistrationAndNormalization(t *testing.T) {
	controller := &fakeResourceAccessController{result: ResourceAccessResult{
		RequestID:     "req_access",
		Status:        ResourceAccessStatusGranted,
		Permission:    ResourcePermissionWrite,
		Source:        ResourceAccessSourceBotOwner,
		ResourceType:  "folder",
		ResourceToken: "fld_root",
	}}
	if got := NewDocsResourceAccessTools(controller, Config{}); len(got) != 0 {
		t.Fatalf("disabled access tools = %d, want 0", len(got))
	}
	tools := NewDocsResourceAccessTools(controller, Config{Docs: DocsToolsConfig{Enabled: true}})
	if len(tools) != 1 || tools[0].Spec().Name != ResourceAccessToolName {
		t.Fatalf("access tools = %#v", toolNamesForTest(tools))
	}
	result := tools[0].Execute(context.Background(), tooltypes.Call{
		ID:   "call_access",
		Name: ResourceAccessToolName,
		Arguments: json.RawMessage(`{
			"resource_type":" folder ",
			"resource_token":" bot_root ",
			"permission":" WRITE ",
			"once_duration_minutes":30,
			"reason":" create a document "
		}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v", result)
	}
	if controller.request.ResourceType != "folder" || controller.request.ResourceToken != BotRootResourceAlias || controller.request.Permission != ResourcePermissionWrite || controller.request.OnceDurationMinutes != 30 || controller.request.Reason != "create a document" {
		t.Fatalf("normalized request = %#v", controller.request)
	}
	if !strings.Contains(result.Content, `"request_id":"req_access"`) || !strings.Contains(result.Content, `"source":"bot_owner"`) {
		t.Fatalf("result content = %s", result.Content)
	}
	if result.PendingWorkflowID != "" {
		t.Fatalf("granted PendingWorkflowID = %q, want empty", result.PendingWorkflowID)
	}
}

func TestDocsResourceAccessToolReturnsPendingWorkflowID(t *testing.T) {
	controller := &fakeResourceAccessController{result: ResourceAccessResult{
		RequestID:     "req_pending",
		Status:        ResourceAccessStatusPending,
		Permission:    ResourcePermissionWrite,
		ResourceType:  "docx",
		ResourceToken: "doxcn_pending",
	}}
	tool := NewDocsResourceAccessTools(controller, Config{Docs: DocsToolsConfig{Enabled: true}})[0]
	result := tool.Execute(context.Background(), tooltypes.Call{
		ID:        "call_access",
		Name:      ResourceAccessToolName,
		Arguments: json.RawMessage(`{"resource_type":"docx","resource_token":"doxcn_pending","permission":"write","once_duration_minutes":45}`),
	})
	if result.IsError || result.PendingWorkflowID != "req_pending" {
		t.Fatalf("Execute result = %#v, want pending workflow req_pending", result)
	}
}

func TestDocsResourceAccessToolRejectsInvalidPermissionAndAliasType(t *testing.T) {
	controller := &fakeResourceAccessController{}
	tool := NewDocsResourceAccessTools(controller, Config{Docs: DocsToolsConfig{Enabled: true}})[0]
	tests := []json.RawMessage{
		json.RawMessage(`{"resource_type":"folder","resource_token":"fld_token","permission":"admin","once_duration_minutes":30}`),
		json.RawMessage(`{"resource_type":"docx","resource_token":"bot_root","permission":"read","once_duration_minutes":30}`),
		json.RawMessage(`{"resource_type":"docx","resource_token":"doxcn_pending","permission":"read","once_duration_minutes":9}`),
		json.RawMessage(`{"resource_type":"docx","resource_token":"doxcn_pending","permission":"read","once_duration_minutes":61}`),
	}
	for _, args := range tests {
		result := tool.Execute(context.Background(), tooltypes.Call{ID: "call", Name: ResourceAccessToolName, Arguments: args})
		if !result.IsError {
			t.Fatalf("Execute(%s) = %#v, want error", args, result)
		}
	}
}

func TestRequireResourceAccessRejectsMismatchedOrMalformedCapability(t *testing.T) {
	ctx := WithActor(context.Background(), Actor{OpenID: "ou_actor", UserID: "u_actor"})
	ctx = WithChatContext(ctx, ChatContext{ChatID: "oc_chat"})
	requirement := ResourceAccessRequirement{
		ResourceType:  "docx",
		ResourceToken: "doxcn_authorized",
		Permission:    ResourcePermissionRead,
	}
	valid := AuthorizedResource{
		AccountID:             "feishu:cli_test",
		ActorOpenID:           "ou_actor",
		ActorUserID:           "u_actor",
		ChatID:                "oc_chat",
		ResourceType:          "docx",
		ResourceToken:         "doxcn_authorized",
		EffectivePermission:   ResourcePermissionWrite,
		GrantMode:             ResourceAccessGrantModeAll,
		CapabilitySubjectType: "user",
		CapabilitySubjectID:   "ou_actor",
		Source:                ResourceAccessSourceExistingGrant,
	}

	if got, err := requireResourceAccess(ctx, staticResourceAccessGuard{authorized: valid}, "feishu:cli_test", requirement); err != nil || got.ResourceToken != valid.ResourceToken {
		t.Fatalf("valid write-for-read capability = %#v err=%v", got, err)
	}
	validOnce := valid
	validOnce.GrantMode = ResourceAccessGrantModeOnce
	validOnce.ExpiresAt = time.Now().Add(time.Hour)
	if got, err := requireResourceAccess(ctx, staticResourceAccessGuard{authorized: validOnce}, "feishu:cli_test", requirement); err != nil || got.GrantMode != ResourceAccessGrantModeOnce {
		t.Fatalf("valid temporary capability = %#v err=%v", got, err)
	}

	tests := map[string]func(*AuthorizedResource){
		"account":         func(got *AuthorizedResource) { got.AccountID = "feishu:other" },
		"actor open id":   func(got *AuthorizedResource) { got.ActorOpenID = "ou_other" },
		"actor user id":   func(got *AuthorizedResource) { got.ActorUserID = "u_other" },
		"chat":            func(got *AuthorizedResource) { got.ChatID = "oc_other" },
		"resource type":   func(got *AuthorizedResource) { got.ResourceType = "folder" },
		"resource token":  func(got *AuthorizedResource) { got.ResourceToken = "doxcn_other" },
		"permission":      func(got *AuthorizedResource) { got.EffectivePermission = "" },
		"grant mode":      func(got *AuthorizedResource) { got.GrantMode = "temporary" },
		"subject type":    func(got *AuthorizedResource) { got.CapabilitySubjectType = "" },
		"subject id":      func(got *AuthorizedResource) { got.CapabilitySubjectID = "" },
		"source":          func(got *AuthorizedResource) { got.Source = "unknown" },
		"all with expiry": func(got *AuthorizedResource) { got.ExpiresAt = time.Now().Add(time.Hour) },
		"expired once": func(got *AuthorizedResource) {
			got.GrantMode = ResourceAccessGrantModeOnce
			got.ExpiresAt = time.Now().Add(-time.Minute)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			authorized := valid
			mutate(&authorized)
			if got, err := requireResourceAccess(ctx, staticResourceAccessGuard{authorized: authorized}, "feishu:cli_test", requirement); err == nil {
				t.Fatalf("requireResourceAccess returned %#v, want malformed capability error", got)
			}
		})
	}

	readOnly := valid
	readOnly.EffectivePermission = ResourcePermissionRead
	writeRequirement := requirement
	writeRequirement.Permission = ResourcePermissionWrite
	if got, err := requireResourceAccess(ctx, staticResourceAccessGuard{authorized: readOnly}, "feishu:cli_test", writeRequirement); err == nil {
		t.Fatalf("read capability satisfied write requirement: %#v", got)
	}
}
