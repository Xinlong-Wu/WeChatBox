package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tooltypes "lingobridge/internal/tools"
)

type fakeResourceAccessController struct {
	request     ResourceAccessRequest
	validation  ResourceAccessValidation
	consumption ResourceAccessValidation
	consumedBy  string
	result      ResourceAccessResult
	err         error
	actor       Actor
	chat        ChatContext
}

func (f *fakeResourceAccessController) RequestAccess(_ context.Context, request ResourceAccessRequest) (ResourceAccessResult, error) {
	f.request = request
	return f.result, f.err
}

func (f *fakeResourceAccessController) ValidateAccess(ctx context.Context, validation ResourceAccessValidation) (ResourceAccessResult, error) {
	f.validation = validation
	f.actor, _ = ActorFromContext(ctx)
	f.chat, _ = ChatContextFromContext(ctx)
	return f.result, f.err
}

func (f *fakeResourceAccessController) ConsumeAccess(ctx context.Context, validation ResourceAccessValidation, consumingRequestID string) (ResourceAccessResult, error) {
	f.consumption = validation
	f.consumedBy = consumingRequestID
	f.actor, _ = ActorFromContext(ctx)
	f.chat, _ = ChatContextFromContext(ctx)
	return f.result, f.err
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
			"reason":" create a document "
		}`),
	})
	if result.IsError {
		t.Fatalf("Execute result = %#v", result)
	}
	if controller.request.ResourceType != "folder" || controller.request.ResourceToken != BotRootResourceAlias || controller.request.Permission != ResourcePermissionWrite || controller.request.Reason != "create a document" {
		t.Fatalf("normalized request = %#v", controller.request)
	}
	if !strings.Contains(result.Content, `"request_id":"req_access"`) || !strings.Contains(result.Content, `"source":"bot_owner"`) {
		t.Fatalf("result content = %s", result.Content)
	}
}

func TestDocsResourceAccessToolRejectsInvalidPermissionAndAliasType(t *testing.T) {
	controller := &fakeResourceAccessController{}
	tool := NewDocsResourceAccessTools(controller, Config{Docs: DocsToolsConfig{Enabled: true}})[0]
	tests := []json.RawMessage{
		json.RawMessage(`{"resource_type":"folder","resource_token":"fld_token","permission":"admin"}`),
		json.RawMessage(`{"resource_type":"docx","resource_token":"bot_root","permission":"read"}`),
	}
	for _, args := range tests {
		result := tool.Execute(context.Background(), tooltypes.Call{ID: "call", Name: ResourceAccessToolName, Arguments: args})
		if !result.IsError {
			t.Fatalf("Execute(%s) = %#v, want error", args, result)
		}
	}
}
