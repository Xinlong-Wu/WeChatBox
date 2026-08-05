package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrConversationConflict is returned when a snapshot is saved against a
// stale conversation revision.
var ErrConversationConflict = errors.New("conversation revision conflict")

// Attachment represents non-text content associated with a chat message.
type Attachment struct {
	Type        string `json:"type"`
	MIMEType    string `json:"mime_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Size        int    `json:"size,omitempty"`
	RefProvider string `json:"ref_provider,omitempty"`
	RefType     string `json:"ref_type,omitempty"`
	RefID       string `json:"ref_id,omitempty"`
	LocalPath   string `json:"local_path,omitempty"`
}

// Message represents a single chat message stored in conversation history.
type Message struct {
	Role        string         `json:"role"`
	Content     string         `json:"content"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	ToolTraces  []ToolTrace    `json:"tool_traces,omitempty"`
	Internal    *InternalEvent `json:"internal_event,omitempty"`
}

// InternalEvent marks a runtime-originated conversation event. User input does
// not populate this field; it is used to make asynchronous workflow resumption
// idempotent even when the process stops after saving the resumed turn.
type InternalEvent struct {
	Kind              string `json:"kind"`
	ID                string `json:"id"`
	CommittedRevision int64  `json:"committed_revision,omitempty"`
}

// ToolTrace is a compact audit record for tool use during one assistant turn.
type ToolTrace struct {
	CallID            string `json:"call_id,omitempty"`
	Name              string `json:"name"`
	Status            string `json:"status"`
	Arguments         string `json:"arguments,omitempty"`
	Result            string `json:"result,omitempty"`
	Error             string `json:"error,omitempty"`
	PendingWorkflowID string `json:"pending_workflow_id,omitempty"`
	DurationMillis    int64  `json:"duration_ms,omitempty"`
}

// ProviderContext stores opaque provider-native context items for one model profile.
type ProviderContext struct {
	Provider string            `json:"provider,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Items    []json.RawMessage `json:"items,omitempty"`
}

// WorkflowResumeReceipt is a model-invisible record of an asynchronously
// resumed turn that committed to the conversation. It keeps retry delivery
// idempotent even after ordinary messages are compacted away.
type WorkflowResumeReceipt struct {
	Assistant         Message `json:"assistant"`
	CommittedRevision int64   `json:"committed_revision"`
	// TextChunks preserves the exact first-delivery UUID-to-text mapping. A nil
	// value identifies a legacy receipt that predates durable chunk storage;
	// newly committed receipts persist an empty or populated JSON array.
	TextChunks []string `json:"text_chunks"`
}

// WorkflowOriginReceipt is a model-invisible proof that the conversation CAS
// containing a pending workflow tool trace committed. It survives compaction
// of the ordinary message list so startup recovery can finish the origin
// continuation commit without guessing from the conversation revision alone.
type WorkflowOriginReceipt struct {
	ToolCallID        string `json:"tool_call_id"`
	ToolName          string `json:"tool_name"`
	CommittedRevision int64  `json:"committed_revision"`
}

// IsEmpty reports whether the context has no provider-owned items to round-trip.
func (c ProviderContext) IsEmpty() bool {
	return len(c.Items) == 0
}

// Conversation is a snapshot of a full conversation (one JSONL line).
type Conversation struct {
	Revision               int64                            `json:"revision"`
	Messages               []Message                        `json:"messages"`
	ProviderContexts       map[string]ProviderContext       `json:"provider_contexts,omitempty"`
	WorkflowResumeReceipts map[string]WorkflowResumeReceipt `json:"workflow_resume_receipts,omitempty"`
	WorkflowOriginReceipts map[string]WorkflowOriginReceipt `json:"workflow_origin_receipts,omitempty"`
}

// SessionDir returns the directory for a user's sessions in this platform store.
func (s *Store) SessionDir(userID string) string {
	return filepath.Join(s.dataDir, "sessions", userID)
}

// SessionPath returns the JSONL file path for a specific session in this platform store.
func (s *Store) SessionPath(userID, sessionID string) string {
	return filepath.Join(s.SessionDir(userID), sessionID+".jsonl")
}

// LoadConversation reads the last line of a JSONL file as the current conversation.
// Returns an empty conversation if the file doesn't exist.
func (s *Store) LoadConversation(userID, sessionID string) (*Conversation, error) {
	return loadConversationFile(s.SessionPath(userID, sessionID))
}

func loadConversationFile(path string) (*Conversation, error) {

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Conversation{}, nil
		}
		return nil, fmt.Errorf("read session file: %w", err)
	}

	// Find the last non-empty line
	lines := splitLines(string(data))
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "" {
			continue
		}
		var conv Conversation
		if err := json.Unmarshal([]byte(lines[i]), &conv); err != nil {
			return nil, fmt.Errorf("parse JSONL line %d: %w", i+1, err)
		}
		if conv.Revision < 0 {
			return nil, fmt.Errorf("parse JSONL line %d: negative conversation revision %d", i+1, conv.Revision)
		}
		return &conv, nil
	}

	return &Conversation{}, nil
}

// SaveConversationCAS writes a conversation snapshot only when expectedRevision
// matches the latest persisted revision. A successful save increments the
// revision and atomically replaces the previous file.
func (s *Store) SaveConversationCAS(userID, sessionID string, expectedRevision int64, conv *Conversation) (int64, error) {
	if expectedRevision < 0 {
		return 0, fmt.Errorf("save conversation: negative expected revision %d", expectedRevision)
	}
	if conv == nil {
		return 0, fmt.Errorf("save conversation: nil snapshot")
	}
	path := s.SessionPath(userID, sessionID)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.beginConversationFileWrite(userID, sessionID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var archived bool
	err = tx.QueryRow(
		`SELECT archived FROM sessions WHERE user_id=? AND id=?`,
		userID,
		sessionID,
	).Scan(&archived)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("check conversation session state: %w", err)
	}
	if err == nil && archived {
		return 0, fmt.Errorf("%w: session %s is archived", ErrSessionNotFound, sessionID)
	}

	current, err := loadConversationFile(path)
	if err != nil {
		return 0, err
	}
	if current.Revision != expectedRevision {
		return current.Revision, fmt.Errorf("%w: expected=%d actual=%d", ErrConversationConflict, expectedRevision, current.Revision)
	}
	nextRevision := expectedRevision + 1
	snapshot := *conv
	snapshot.Revision = nextRevision

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return current.Revision, fmt.Errorf("create session dir: %w", err)
	}

	line, err := json.Marshal(&snapshot)
	if err != nil {
		return current.Revision, fmt.Errorf("marshal conversation: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return current.Revision, fmt.Errorf("create session temp file: %w", err)
	}
	tmp := tmpFile.Name()
	removeTemp := true
	defer func() {
		_ = tmpFile.Close()
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := tmpFile.Write(append(line, '\n')); err != nil {
		return current.Revision, fmt.Errorf("write session temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return current.Revision, fmt.Errorf("close session temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return current.Revision, fmt.Errorf("rename session file: %w", err)
	}
	removeTemp = false
	// The transaction only coordinates file writers. Once rename succeeds the
	// conversation is durable enough for subsequent CAS readers even if the
	// advisory lock transaction itself cannot be committed.
	_ = tx.Commit()

	conv.Revision = nextRevision
	return nextRevision, nil
}

// TruncateConversation removes all history for a session in this platform store.
func (s *Store) TruncateConversation(userID, sessionID string) error {
	path := s.SessionPath(userID, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.beginConversationFileWrite(userID, sessionID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Just delete the file; next append will recreate it
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("truncate session: %w", err)
	}
	_ = tx.Commit()
	return nil
}

func (s *Store) beginConversationFileWrite(userID, sessionID string) (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin conversation file write: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO conversation_file_locks (user_id, session_id, generation)
		 VALUES (?, ?, 1)
		 ON CONFLICT(user_id, session_id)
		 DO UPDATE SET generation=conversation_file_locks.generation+1`,
		userID,
		sessionID,
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("acquire conversation file write lock: %w", err)
	}
	return tx, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
