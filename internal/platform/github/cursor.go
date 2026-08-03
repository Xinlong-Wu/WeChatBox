package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	cursorStatusReviewed            = "reviewed"
	cursorStatusMissingInstructions = "missing_instructions"
)

type cursorStore interface {
	GetSyncBuf(accountID string) (string, error)
	SaveSyncBuf(accountID, buf string) error
}

type cursorState struct {
	PRs map[string]cursorEntry `json:"prs"`
}

type cursorEntry struct {
	HeadSHA          string `json:"head_sha"`
	Status           string `json:"status"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	LastCommentCheck string `json:"last_comment_check,omitempty"`
}

func loadCursor(st cursorStore, accountID string) (cursorState, error) {
	buf, err := st.GetSyncBuf(accountID)
	if err != nil {
		return cursorState{}, err
	}
	if strings.TrimSpace(buf) == "" {
		return cursorState{PRs: map[string]cursorEntry{}}, nil
	}
	var state cursorState
	if err := json.Unmarshal([]byte(buf), &state); err != nil {
		return cursorState{}, fmt.Errorf("parse github cursor: %w", err)
	}
	if state.PRs == nil {
		state.PRs = map[string]cursorEntry{}
	}
	return state, nil
}

func saveCursor(st cursorStore, accountID string, state cursorState) error {
	if state.PRs == nil {
		state.PRs = map[string]cursorEntry{}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal github cursor: %w", err)
	}
	return st.SaveSyncBuf(accountID, string(data))
}

func cursorKey(pr PullRequest) string {
	return fmt.Sprintf("%s#%d", pr.Base.Repo.FullName(), pr.Number)
}

func shouldProcessCursor(state cursorState, pr PullRequest) bool {
	entry, ok := state.PRs[cursorKey(pr)]
	if !ok {
		return true
	}
	if strings.TrimSpace(entry.HeadSHA) != strings.TrimSpace(pr.Head.SHA) {
		return true
	}
	return entry.Status != cursorStatusReviewed && entry.Status != cursorStatusMissingInstructions
}

func markCursor(state cursorState, pr PullRequest, status string, now time.Time) cursorState {
	if state.PRs == nil {
		state.PRs = map[string]cursorEntry{}
	}
	nowStr := now.UTC().Format(time.RFC3339)
	state.PRs[cursorKey(pr)] = cursorEntry{
		HeadSHA:          strings.TrimSpace(pr.Head.SHA),
		Status:           status,
		UpdatedAt:        nowStr,
		LastCommentCheck: nowStr,
	}
	return state
}

// commentCheckSince returns the effective "since" time for comment polling.
// It prefers LastCommentCheck, falls back to UpdatedAt.
func commentCheckSince(entry cursorEntry) time.Time {
	if s := strings.TrimSpace(entry.LastCommentCheck); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	if s := strings.TrimSpace(entry.UpdatedAt); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// markCommentCheck updates only the LastCommentCheck timestamp without
// changing status or HeadSHA.
func markCommentCheck(state cursorState, pr PullRequest, now time.Time) cursorState {
	if state.PRs == nil {
		state.PRs = map[string]cursorEntry{}
	}
	key := cursorKey(pr)
	entry := state.PRs[key]
	entry.LastCommentCheck = now.UTC().Format(time.RFC3339)
	state.PRs[key] = entry
	return state
}
