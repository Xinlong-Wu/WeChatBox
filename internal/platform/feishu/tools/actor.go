package tools

import (
	"context"
	"strings"

	tooltypes "lingobridge/internal/tools"
)

// Actor describes the Feishu user who triggered a tool call.
type Actor struct {
	OpenID string
	UserID string
	Name   string
	Email  string
}

// WithActor attaches sanitized Feishu sender identity to a tool execution context.
func WithActor(ctx context.Context, actor Actor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	actor = normalizeActor(actor)
	if actor.OpenID == "" && actor.UserID == "" && actor.Name == "" && actor.Email == "" {
		return ctx
	}
	execution, _ := tooltypes.ExecutionContextFromContext(ctx)
	execution.ActorOpenID = actor.OpenID
	execution.ActorUserID = actor.UserID
	execution.ActorName = actor.Name
	execution.ActorEmail = actor.Email
	return tooltypes.WithExecutionContext(ctx, execution)
}

// ActorFromContext returns the Feishu sender attached by WithActor.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	if ctx == nil {
		return Actor{}, false
	}
	execution, ok := tooltypes.ExecutionContextFromContext(ctx)
	if !ok {
		return Actor{}, false
	}
	actor := Actor{
		OpenID: execution.ActorOpenID,
		UserID: execution.ActorUserID,
		Name:   execution.ActorName,
		Email:  execution.ActorEmail,
	}
	actor = normalizeActor(actor)
	return actor, actor.OpenID != "" || actor.UserID != "" || actor.Name != "" || actor.Email != ""
}

func normalizeActor(actor Actor) Actor {
	actor.OpenID = strings.TrimSpace(actor.OpenID)
	actor.UserID = strings.TrimSpace(actor.UserID)
	actor.Name = strings.TrimSpace(actor.Name)
	actor.Email = strings.TrimSpace(actor.Email)
	return actor
}
