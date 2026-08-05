package session

import (
	"strings"
	"testing"

	"lingobridge/internal/config"
	"lingobridge/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	st, err := store.Open(store.PlatformWeChat)
	if err != nil {
		t.Fatalf("store.Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return NewManager(st), st
}

func TestArchiveCurrentSessionSelectsFallback(t *testing.T) {
	manager, _ := newTestManager(t)

	if _, err := manager.CreateSession("user", "work"); err != nil {
		t.Fatalf("CreateSession work returned error: %v", err)
	}
	if _, err := manager.CreateSession("user", "play"); err != nil {
		t.Fatalf("CreateSession play returned error: %v", err)
	}

	result, err := manager.ArchiveSession("user", "")
	if err != nil {
		t.Fatalf("ArchiveSession returned error: %v", err)
	}
	if result.Archived.Name != "play" {
		t.Fatalf("archived session = %s, want play", result.Archived.Name)
	}
	if !result.CurrentChanged || result.Current == nil || result.Current.Name != "work" {
		t.Fatalf("archive result = %#v, want fallback to work", result)
	}
}

func TestClearSessionArchivesCurrentAndCreatesNewCurrent(t *testing.T) {
	manager, _ := newTestManager(t)

	if _, err := manager.CreateSession("user", "work"); err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}

	next, err := manager.ClearSession("user")
	if err != nil {
		t.Fatalf("ClearSession returned error: %v", err)
	}
	if !next.Current || !strings.HasPrefix(next.Name, "session-") {
		t.Fatalf("next session = %#v, want generated current session", next)
	}

	current, err := manager.CurrentSession("user")
	if err != nil {
		t.Fatalf("CurrentSession returned error: %v", err)
	}
	if current.ID != next.ID {
		t.Fatalf("current session = %#v, want clear result %#v", current, next)
	}

	sessions, err := manager.ListSessions("user")
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != next.ID || sessions[0].Name == "work" {
		t.Fatalf("sessions = %#v, want only new unarchived session", sessions)
	}
}

func TestClearSessionWithoutExistingSessionCreatesNewCurrent(t *testing.T) {
	manager, _ := newTestManager(t)

	next, err := manager.ClearSession("user")
	if err != nil {
		t.Fatalf("ClearSession returned error: %v", err)
	}
	if !next.Current || !strings.HasPrefix(next.Name, "session-") {
		t.Fatalf("next session = %#v, want generated current session", next)
	}

	sessions, err := manager.ListSessions("user")
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != next.ID {
		t.Fatalf("sessions = %#v, want only new current session", sessions)
	}
}

func TestCurrentModelFallsBackToDefault(t *testing.T) {
	_, st := newTestManager(t)
	manager := NewManager(st, config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {},
			"gpt4o":    {},
		},
	})
	sess, err := st.CreateSession("user", "work")
	if err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}
	if err := st.SetSessionModelName("user", sess.ID, "missing"); err != nil {
		t.Fatalf("SetSessionModelName returned error: %v", err)
	}

	model, err := manager.CurrentModel("user", sess.ID)
	if err != nil {
		t.Fatalf("CurrentModel returned error: %v", err)
	}
	if model != "deepseek" {
		t.Fatalf("CurrentModel = %q, want deepseek", model)
	}
}

func TestModelPreferenceIsScopedToSession(t *testing.T) {
	_, st := newTestManager(t)
	manager := NewManager(st, config.LLMConfig{
		DefaultModel: "deepseek",
		Models: map[string]config.LLMModelConfig{
			"deepseek": {},
			"gpt4o":    {},
		},
	})
	work, err := manager.CreateSession("user", "work")
	if err != nil {
		t.Fatalf("CreateSession work returned error: %v", err)
	}
	play, err := manager.CreateSession("user", "play")
	if err != nil {
		t.Fatalf("CreateSession play returned error: %v", err)
	}
	if err := manager.SetModel("user", work.ID, "gpt4o"); err != nil {
		t.Fatalf("SetModel returned error: %v", err)
	}

	workModel, err := manager.CurrentModel("user", work.ID)
	if err != nil {
		t.Fatalf("CurrentModel work returned error: %v", err)
	}
	playModel, err := manager.CurrentModel("user", play.ID)
	if err != nil {
		t.Fatalf("CurrentModel play returned error: %v", err)
	}
	if workModel != "gpt4o" || playModel != "deepseek" {
		t.Fatalf("models = work:%q play:%q, want gpt4o/deepseek", workModel, playModel)
	}
}
