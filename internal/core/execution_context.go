package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	tooltypes "lingobridge/internal/tools"
)

func bindToolExecutionContext(ctx context.Context, msg InboundMessage, sessionID string) (context.Context, tooltypes.ExecutionContext, error) {
	execution, _ := tooltypes.ExecutionContextFromContext(ctx)
	turnID, err := newTurnID()
	if err != nil {
		return ctx, tooltypes.ExecutionContext{}, err
	}
	execution.Platform = msg.Platform
	execution.AccountID = msg.AccountID
	execution.UserKey = msg.UserKey
	execution.SessionID = sessionID
	execution.ConversationRevision = 0
	execution.TurnID = turnID
	execution.ToolCallID = ""
	execution.ToolName = ""
	ctx = tooltypes.WithExecutionContext(ctx, execution)
	execution, _ = tooltypes.ExecutionContextFromContext(ctx)
	return ctx, execution, nil
}

func newTurnID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate turn id: %w", err)
	}
	return "turn_" + hex.EncodeToString(random[:]), nil
}
