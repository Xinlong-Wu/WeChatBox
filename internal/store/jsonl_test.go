package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveConversationCASIncrementsRevisionAndRejectsStaleSnapshot(t *testing.T) {
	st := openTestStore(t)
	first := &Conversation{Messages: []Message{{Role: "user", Content: "first"}}}
	revision, err := st.SaveConversationCAS("user", "session", 0, first)
	if err != nil {
		t.Fatalf("SaveConversationCAS first returned error: %v", err)
	}
	if revision != 1 || first.Revision != 1 {
		t.Fatalf("first revision = return:%d snapshot:%d, want 1", revision, first.Revision)
	}

	stale := &Conversation{Messages: []Message{{Role: "user", Content: "stale"}}}
	actualRevision, err := st.SaveConversationCAS("user", "session", 0, stale)
	if !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("SaveConversationCAS stale error = %v, want ErrConversationConflict", err)
	}
	if actualRevision != 1 || stale.Revision != 0 {
		t.Fatalf("stale revision = return:%d snapshot:%d, want 1/0", actualRevision, stale.Revision)
	}

	loaded, err := st.LoadConversation("user", "session")
	if err != nil {
		t.Fatalf("LoadConversation returned error: %v", err)
	}
	if loaded.Revision != 1 || len(loaded.Messages) != 1 || loaded.Messages[0].Content != "first" {
		t.Fatalf("loaded = %#v, want first snapshot at revision 1", loaded)
	}
}

func TestSaveConversationCASAllowsOnlyOneConcurrentWriter(t *testing.T) {
	st := openTestStore(t)
	start := make(chan struct{})
	type saveResult struct {
		revision int64
		err      error
	}
	results := make(chan saveResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, content := range []string{"one", "two"} {
		content := content
		go func() {
			ready.Done()
			<-start
			revision, err := st.SaveConversationCAS("user", "session", 0, &Conversation{
				Messages: []Message{{Role: "user", Content: content}},
			})
			results <- saveResult{revision: revision, err: err}
		}()
	}
	ready.Wait()
	close(start)

	succeeded := 0
	conflicted := 0
	for i := 0; i < 2; i++ {
		result := <-results
		switch {
		case result.err == nil && result.revision == 1:
			succeeded++
		case errors.Is(result.err, ErrConversationConflict) && result.revision == 1:
			conflicted++
		default:
			t.Fatalf("unexpected save result = %#v", result)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("save outcomes = succeeded:%d conflicted:%d, want 1/1", succeeded, conflicted)
	}
}

func TestLoadConversationTreatsLegacySnapshotAsRevisionZero(t *testing.T) {
	st := openTestStore(t)
	path := st.SessionPath("user", "legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"messages":[{"role":"user","content":"legacy"}]}`+"\n"), 0600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	loaded, err := st.LoadConversation("user", "legacy")
	if err != nil {
		t.Fatalf("LoadConversation returned error: %v", err)
	}
	if loaded.Revision != 0 || len(loaded.Messages) != 1 || loaded.Messages[0].Content != "legacy" {
		t.Fatalf("legacy snapshot = %#v", loaded)
	}
	revision, err := st.SaveConversationCAS("user", "legacy", 0, loaded)
	if err != nil {
		t.Fatalf("SaveConversationCAS legacy returned error: %v", err)
	}
	if revision != 1 || loaded.Revision != 1 {
		t.Fatalf("legacy revision = return:%d snapshot:%d, want 1", revision, loaded.Revision)
	}
}
