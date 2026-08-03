package github

import (
	"strings"
)

const (
	commandReview = "review"
	commandBot    = "bot"
)

// CommentCommand represents a parsed bot command from a PR comment.
type CommentCommand struct {
	Type    string // "review", "bot", or "" (no command)
	Message string // for /bot, the message text; empty for /review
}

// parseCommentCommand extracts a bot command from a comment body.
// It looks at the first line for /review or /bot <message>.
func parseCommentCommand(body string) CommentCommand {
	body = strings.TrimSpace(body)
	if body == "" {
		return CommentCommand{}
	}

	// Take the first line for command detection.
	firstLine := body
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		firstLine = body[:idx]
	}
	firstLine = strings.TrimSpace(firstLine)
	if firstLine == "" {
		return CommentCommand{}
	}

	lower := strings.ToLower(firstLine)

	// Match /review (with optional trailing text ignored).
	if lower == "/review" || strings.HasPrefix(lower, "/review ") {
		return CommentCommand{Type: commandReview}
	}

	// Match /bot <message> — requires a non-empty message.
	if strings.HasPrefix(lower, "/bot ") {
		msg := strings.TrimSpace(firstLine[len("/bot "):])
		if msg == "" {
			return CommentCommand{}
		}
		// For multi-line /bot messages, include the full body after /bot.
		if idx := strings.IndexByte(body, '\n'); idx >= 0 {
			fullMsg := strings.TrimSpace(body[len("/bot "):])
			if fullMsg != "" {
				msg = fullMsg
			}
		}
		return CommentCommand{Type: commandBot, Message: msg}
	}
	if lower == "/bot" {
		return CommentCommand{}
	}

	return CommentCommand{}
}
