package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSaveConversationCASIncrementsRevisionAndRejectsStaleSnapshot(t *testing.T) {
	st := openTestStore(t)
	first := &Conversation{
		Messages: []Message{{Role: "user", Content: "first"}},
		WorkflowOriginReceipts: map[string]WorkflowOriginReceipt{
			"req_origin": {
				ToolCallID:        "call_origin",
				ToolName:          "feishu_docs_request_access",
				CommittedRevision: 1,
			},
		},
		WorkflowResumeReceipts: map[string]WorkflowResumeReceipt{
			"req_resume": {
				Assistant:         Message{Role: "assistant", Content: "resumed"},
				CommittedRevision: 1,
				TextChunks:        []string{"res", "umed"},
			},
		},
	}
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
	if receipt := loaded.WorkflowResumeReceipts["req_resume"]; receipt.CommittedRevision != 1 || receipt.Assistant.Content != "resumed" ||
		len(receipt.TextChunks) != 2 || receipt.TextChunks[0] != "res" || receipt.TextChunks[1] != "umed" {
		t.Fatalf("loaded workflow resume receipt = %#v", receipt)
	}
	if receipt := loaded.WorkflowOriginReceipts["req_origin"]; receipt.CommittedRevision != 1 ||
		receipt.ToolCallID != "call_origin" || receipt.ToolName != "feishu_docs_request_access" {
		t.Fatalf("loaded workflow origin receipt = %#v", receipt)
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

func TestSaveConversationCASAcrossStoresAllowsOnlyOneConcurrentWriter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, err := Open(PlatformWeChat)
	if err != nil {
		t.Fatalf("Open first store returned error: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(PlatformWeChat)
	if err != nil {
		t.Fatalf("Open second store returned error: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	start := make(chan struct{})
	type saveResult struct {
		revision int64
		err      error
	}
	results := make(chan saveResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index, candidate := range []*Store{first, second} {
		index, candidate := index, candidate
		go func() {
			ready.Done()
			<-start
			revision, saveErr := candidate.SaveConversationCAS("user", "shared-session", 0, &Conversation{
				Messages: []Message{{Role: "user", Content: strings.Repeat(string(rune('a'+index)), 4<<20)}},
			})
			results <- saveResult{revision: revision, err: saveErr}
		}()
	}
	ready.Wait()
	close(start)

	succeeded := 0
	conflicted := 0
	for index := 0; index < 2; index++ {
		result := <-results
		switch {
		case result.err == nil && result.revision == 1:
			succeeded++
		case errors.Is(result.err, ErrConversationConflict) && result.revision == 1:
			conflicted++
		default:
			t.Fatalf("unexpected cross-store save result = %#v", result)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("cross-store save outcomes = succeeded:%d conflicted:%d, want 1/1", succeeded, conflicted)
	}
	loaded, err := first.LoadConversation("user", "shared-session")
	if err != nil {
		t.Fatalf("LoadConversation returned error: %v", err)
	}
	if loaded.Revision != 1 || len(loaded.Messages) != 1 || len(loaded.Messages[0].Content) != 4<<20 {
		t.Fatalf("loaded cross-store conversation revision/messages = %d/%d content_len=%d", loaded.Revision, len(loaded.Messages), len(loaded.Messages[0].Content))
	}
}

func TestSaveConversationCASRejectsArchivedSession(t *testing.T) {
	st := openTestStore(t)
	sess, err := st.CreateSession("user", "archived-target")
	if err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}
	conv := &Conversation{Messages: []Message{{Role: "user", Content: "before archive"}}}
	if _, err := st.SaveConversationCAS(sess.UserID, sess.ID, 0, conv); err != nil {
		t.Fatalf("initial SaveConversationCAS returned error: %v", err)
	}
	if _, err := st.ArchiveSession(sess.UserID, sess.Name); err != nil {
		t.Fatalf("ArchiveSession returned error: %v", err)
	}
	conv.Messages = append(conv.Messages, Message{Role: "assistant", Content: "must not be saved"})
	if _, err := st.SaveConversationCAS(sess.UserID, sess.ID, conv.Revision, conv); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SaveConversationCAS archived error = %v, want ErrSessionNotFound", err)
	}
	loaded, err := st.LoadConversation(sess.UserID, sess.ID)
	if err != nil {
		t.Fatalf("LoadConversation returned error: %v", err)
	}
	if loaded.Revision != 1 || len(loaded.Messages) != 1 {
		t.Fatalf("archived conversation changed = %#v", loaded)
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
