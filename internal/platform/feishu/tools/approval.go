package tools

import (
	"context"
	"encoding/json"
	"time"
)

// ApprovalField is one sanitized, human-readable fact shown on an authorization card.
// Payload data that should not be displayed, such as document content, must not be put here.
type ApprovalField struct {
	Label string
	Value string
}

const (
	OperationApprovalStatusGranted = "granted"
	OperationApprovalStatusPending = "pending"
)

// OperationApprovalPolicy is owned by one tool action and controls how the
// shared approval service presents and routes that operation.
type OperationApprovalPolicy struct {
	ToolName    string
	ActionKey   string
	Action      string
	SupportsAll bool
}

// OperationApprovalRequest asks the shared service to check reusable approval
// or create one durable card workflow for an exact resource and payload.
type OperationApprovalRequest struct {
	ToolName      string
	ActionKey     string
	ResourceType  string
	ResourceToken string
	Fields        []ApprovalField
	Payload       json.RawMessage
}

// OperationApprovalResult reports whether the operation may execute now or is
// waiting for a durable Feishu card decision.
type OperationApprovalResult struct {
	Status    string
	RequestID string
	ExpiresAt time.Time
}

// OperationApprovalExecution is the user-facing result of an approved
// asynchronous tool operation.
type OperationApprovalExecution struct {
	Message       string
	Warning       bool
	WarningReason string
}

// OperationApprovalService is the single tool-facing entry point for reusable
// operation authorization and durable approval-card creation.
type OperationApprovalService interface {
	CheckOrRequest(ctx context.Context, request OperationApprovalRequest) (OperationApprovalResult, error)
}

// OperationApprovalExecutor owns one operation policy and executes the exact
// payload persisted before the requesting user approved it.
type OperationApprovalExecutor interface {
	OperationApprovalPolicy() OperationApprovalPolicy
	ExecuteApproved(ctx context.Context, requestID string, payload json.RawMessage) (OperationApprovalExecution, error)
}
