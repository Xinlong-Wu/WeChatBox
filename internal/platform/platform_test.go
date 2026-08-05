package platform

import (
	"context"
	"reflect"
	"testing"

	"lingobridge/internal/commands"
	"lingobridge/internal/core"
	"lingobridge/internal/store"
)

type policyCapturePlatform struct {
	handler core.Handler
}

func (p *policyCapturePlatform) Run(_ context.Context, handler core.Handler) error {
	p.handler = handler
	return nil
}

type policyRecordingHandler struct {
	message core.InboundMessage
}

func (h *policyRecordingHandler) Handle(_ context.Context, message core.InboundMessage, _ core.Sender) error {
	h.message = message
	return nil
}

type policyRecordingWorkflowHandler struct {
	policyRecordingHandler
	resumeContext context.Context
	resumeRequest core.WorkflowResumeRequest
	resumeSender  core.Sender
	resumeCalls   int
}

func (h *policyRecordingWorkflowHandler) ResumeWorkflow(ctx context.Context, request core.WorkflowResumeRequest, sender core.Sender) error {
	h.resumeContext = ctx
	h.resumeRequest = request
	h.resumeSender = sender
	h.resumeCalls++
	return nil
}

type policyTestSender struct{}

func (*policyTestSender) Send(context.Context, core.OutboundMessage) error { return nil }

func (*policyTestSender) StartTyping(context.Context) func() { return func() {} }

func TestPolicyPlatformPreservesWorkflowResumer(t *testing.T) {
	captured := &policyCapturePlatform{}
	policy := commands.PolicyWithDisabled("/model")
	definition := Definition{
		NewRuntimePlatform: func(RuntimeContext) (core.Platform, error) {
			return captured, nil
		},
		CommandPolicy: policy,
	}
	runtime, err := definition.RuntimePlatform(RuntimeContext{})
	if err != nil {
		t.Fatalf("RuntimePlatform returned error: %v", err)
	}
	handler := &policyRecordingWorkflowHandler{}
	ctx := context.WithValue(t.Context(), struct{}{}, "resume-context")
	if err := runtime.Run(ctx, handler); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if captured.handler == nil {
		t.Fatal("inner platform did not receive a handler")
	}
	resumer, ok := captured.handler.(core.WorkflowResumer)
	if !ok {
		t.Fatalf("policy-wrapped handler type %T lost core.WorkflowResumer", captured.handler)
	}
	sender := &policyTestSender{}
	request := core.WorkflowResumeRequest{
		Continuation: store.WorkflowContinuation{RequestID: "req_policy_resume", AccountID: "feishu:cli_test"},
		Result:       store.WorkflowResult{RequestID: "req_policy_resume", AccountID: "feishu:cli_test"},
		AccountName:  "default",
	}
	if err := resumer.ResumeWorkflow(ctx, request, sender); err != nil {
		t.Fatalf("ResumeWorkflow returned error: %v", err)
	}
	if handler.resumeCalls != 1 || handler.resumeContext != ctx || handler.resumeSender != sender || !reflect.DeepEqual(handler.resumeRequest, request) {
		t.Fatalf("resume forwarding calls=%d context_match=%t sender_match=%t request=%#v", handler.resumeCalls, handler.resumeContext == ctx, handler.resumeSender == sender, handler.resumeRequest)
	}
	if err := captured.handler.Handle(ctx, core.InboundMessage{}, sender); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if handler.message.CommandPolicy.Allows("/model") || !handler.message.CommandPolicy.Allows("/help") {
		t.Fatalf("forwarded command policy = %#v", handler.message.CommandPolicy)
	}
}

func TestPolicyPlatformDoesNotAdvertiseUnsupportedWorkflowResumer(t *testing.T) {
	captured := &policyCapturePlatform{}
	definition := Definition{
		NewRuntimePlatform: func(RuntimeContext) (core.Platform, error) {
			return captured, nil
		},
		CommandPolicy: commands.DefaultPolicy(),
	}
	runtime, err := definition.RuntimePlatform(RuntimeContext{})
	if err != nil {
		t.Fatalf("RuntimePlatform returned error: %v", err)
	}
	if err := runtime.Run(t.Context(), &policyRecordingHandler{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, ok := captured.handler.(core.WorkflowResumer); ok {
		t.Fatalf("policy-wrapped handler type %T unexpectedly implements core.WorkflowResumer", captured.handler)
	}
}

var _ core.Handler = (*policyRecordingHandler)(nil)
var _ core.WorkflowResumer = (*policyRecordingWorkflowHandler)(nil)
var _ core.Sender = (*policyTestSender)(nil)
