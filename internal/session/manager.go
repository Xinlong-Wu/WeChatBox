package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"lingobridge/internal/config"
	"lingobridge/internal/store"
)

// ErrModelNotFound is returned when a user selects an unknown model profile.
var ErrModelNotFound = errors.New("model not found")

// Manager handles multi-user, multi-session conversation management.
type Manager struct {
	store        *store.Store
	defaultModel string
	models       map[string]bool
	modelNames   []string
}

// NewManager creates a new session manager.
func NewManager(st *store.Store, cfgs ...config.LLMConfig) *Manager {
	m := &Manager{store: st}
	if len(cfgs) > 0 {
		cfg := cfgs[0]
		m.defaultModel = cfg.DefaultModel
		m.modelNames = cfg.ModelNames()
		m.models = make(map[string]bool, len(m.modelNames))
		for _, name := range m.modelNames {
			m.models[name] = true
		}
	}
	return m
}

// GetOrCreateCurrentSession returns the current session for a user.
// Creates a default session if none exists.
func (m *Manager) GetOrCreateCurrentSession(userID string) (*store.Session, error) {
	return m.store.GetCurrentSession(userID)
}

// CurrentSession returns the current session for command display.
func (m *Manager) CurrentSession(userID string) (*store.Session, error) {
	return m.GetOrCreateCurrentSession(userID)
}

// LoadHistory loads the conversation history for a session.
func (m *Manager) LoadHistory(userID, sessionID string) (*store.Conversation, error) {
	return m.store.LoadConversation(userID, sessionID)
}

// SaveHistoryCAS saves a conversation snapshot if its expected revision is current.
func (m *Manager) SaveHistoryCAS(userID, sessionID string, expectedRevision int64, conv *store.Conversation) (int64, error) {
	return m.store.SaveConversationCAS(userID, sessionID, expectedRevision, conv)
}

// CreateSession creates a new session for a user and sets it as current.
func (m *Manager) CreateSession(userID, name string) (*store.Session, error) {
	if name == "" {
		name = generatedSessionName()
	}
	return m.store.CreateSession(userID, name)
}

// ListSessions returns all sessions for a user.
func (m *Manager) ListSessions(userID string) ([]store.Session, error) {
	return m.store.ListSessions(userID)
}

// SwitchSession switches the current session for a user.
func (m *Manager) SwitchSession(userID, sessionName string) (*store.Session, error) {
	return m.store.SwitchSession(userID, sessionName)
}

// RenameCurrentSession renames the current session.
func (m *Manager) RenameCurrentSession(userID, newName string) (*store.Session, error) {
	current, err := m.GetOrCreateCurrentSession(userID)
	if err != nil {
		return nil, err
	}
	return m.store.RenameSession(userID, current.ID, newName)
}

// ArchiveSession archives a named session, or the current session if name is empty.
func (m *Manager) ArchiveSession(userID, name string) (*store.ArchiveResult, error) {
	current, err := m.GetOrCreateCurrentSession(userID)
	if err != nil {
		return nil, err
	}
	targetName := name
	if targetName == "" {
		targetName = current.Name
	}

	archived, err := m.store.ArchiveSession(userID, targetName)
	if err != nil {
		return nil, err
	}

	result := &store.ArchiveResult{Archived: *archived}
	if archived.ID == current.ID {
		next, err := m.GetOrCreateCurrentSession(userID)
		if err != nil {
			return nil, err
		}
		result.Current = next
		result.CurrentChanged = true
	} else {
		result.Current = current
	}
	return result, nil
}

// ClearSession archives the current session and creates a new current session.
func (m *Manager) ClearSession(userID string) (*store.Session, error) {
	return m.store.ClearCurrentSession(userID, generatedSessionName())
}

// CurrentModel returns the model profile for a session, falling back to default.
func (m *Manager) CurrentModel(userID, sessionID string) (string, error) {
	modelName, err := m.store.GetSessionModelName(userID, sessionID)
	if err != nil {
		return "", err
	}
	if modelName != "" && m.HasModel(modelName) {
		return modelName, nil
	}
	return m.defaultModel, nil
}

// SetModel saves a model profile preference for one session.
func (m *Manager) SetModel(userID, sessionID, modelName string) error {
	if !m.HasModel(modelName) {
		return fmt.Errorf("%w: %q", ErrModelNotFound, modelName)
	}
	return m.store.SetSessionModelName(userID, sessionID, modelName)
}

// DefaultModelName returns the configured default model profile.
func (m *Manager) DefaultModelName() string {
	return m.defaultModel
}

// ListModels returns sorted model profile names.
func (m *Manager) ListModels() []string {
	names := make([]string, len(m.modelNames))
	copy(names, m.modelNames)
	return names
}

// HasModel reports whether a model profile exists.
func (m *Manager) HasModel(modelName string) bool {
	return m.models[modelName]
}

func generatedSessionName() string {
	return fmt.Sprintf("session-%d", time.Now().Unix())
}

// FormatSessionList formats sessions for display.
func FormatSessionList(sessions []store.Session) string {
	var sb strings.Builder
	sb.WriteString("你的会话列表：\n")
	for _, s := range sessions {
		marker := "  "
		if s.Current {
			marker = "▶ "
		}
		sb.WriteString(fmt.Sprintf("%s%s (创建于 %s)\n", marker, s.Name, s.CreatedAt))
	}
	return sb.String()
}
