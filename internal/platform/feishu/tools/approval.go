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

// ApprovalRequest asks the Feishu frontend to authorize one exact tool payload.
type ApprovalRequest struct {
	ToolName string
	Action   string
	Fields   []ApprovalField
	Payload  json.RawMessage
}

// PendingApproval identifies a durable approval request sent to Feishu.
type PendingApproval struct {
	RequestID string
	ExpiresAt time.Time
}

// ApprovalExecution is the user-facing result of an approved asynchronous tool operation.
type ApprovalExecution struct {
	Message       string
	Warning       bool
	WarningReason string
}

// ApprovalRequester checks reusable authorization and creates a Feishu approval request when needed.
type ApprovalRequester interface {
	HasActiveGrant(ctx context.Context, toolName string) (bool, error)
	RequestApproval(ctx context.Context, request ApprovalRequest) (PendingApproval, error)
}

// ApprovalExecutor executes a previously validated payload after the requesting user approves it.
type ApprovalExecutor interface {
	ApprovalToolName() string
	ExecuteApproved(ctx context.Context, requestID string, payload json.RawMessage) (ApprovalExecution, error)
}
