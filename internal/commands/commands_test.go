package commands

import (
	"fmt"
	"strings"
	"testing"

	"lingobridge/internal/session"
	"lingobridge/internal/store"
)

type fakeSessionManager struct {
	createErr    error
	switchErr    error
	renameErr    error
	archiveErr   error
	clearErr     error
	setModelErr  error
	sessions     []store.Session
	currentModel string
	models       []string
	clearCalled  bool
	modelSession string
	setSession   string
}

func (f *fakeSessionManager) CurrentSession(userID string) (*store.Session, error) {
	return &store.Session{ID: "current", UserID: userID, Name: "default", Current: true}, nil
}

func (f *fakeSessionManager) CreateSession(userID, name string) (*store.Session, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &store.Session{ID: "new", UserID: userID, Name: name, Current: true}, nil
}

func (f *fakeSessionManager) ListSessions(userID string) ([]store.Session, error) {
	return f.sessions, nil
}

func (f *fakeSessionManager) SwitchSession(userID, sessionName string) (*store.Session, error) {
	if f.switchErr != nil {
		return nil, f.switchErr
	}
	return &store.Session{ID: "switched", UserID: userID, Name: sessionName, Current: true}, nil
}

func (f *fakeSessionManager) RenameCurrentSession(userID, newName string) (*store.Session, error) {
	if f.renameErr != nil {
		return nil, f.renameErr
	}
	return &store.Session{ID: "current", UserID: userID, Name: newName, Current: true}, nil
}

func (f *fakeSessionManager) ArchiveSession(userID, sessionName string) (*store.ArchiveResult, error) {
	if f.archiveErr != nil {
		return nil, f.archiveErr
	}
	return &store.ArchiveResult{
		Archived:       store.Session{ID: "current", UserID: userID, Name: "default", Archived: true},
		Current:        &store.Session{ID: "next", UserID: userID, Name: "next", Current: true},
		CurrentChanged: true,
	}, nil
}

func (f *fakeSessionManager) ClearSession(userID string) (*store.Session, error) {
	f.clearCalled = true
	if f.clearErr != nil {
		return nil, f.clearErr
	}
	return &store.Session{ID: "cleared", UserID: userID, Name: "session-1", Current: true}, nil
}

func (f *fakeSessionManager) CurrentModel(userID, sessionID string) (string, error) {
	f.modelSession = sessionID
	if f.currentModel != "" {
		return f.currentModel, nil
	}
	return "deepseek", nil
}

func (f *fakeSessionManager) SetModel(userID, sessionID, modelName string) error {
	f.setSession = sessionID
	if f.setModelErr != nil {
		return f.setModelErr
	}
	return nil
}

func (f *fakeSessionManager) DefaultModelName() string {
	return "deepseek"
}

func (f *fakeSessionManager) ListModels() []string {
	if len(f.models) > 0 {
		return f.models
	}
	return []string{"deepseek", "gpt4o"}
}

func TestHandleNewDuplicateSession(t *testing.T) {
	manager := &fakeSessionManager{
		createErr: fmt.Errorf("%w: work", store.ErrSessionExists),
	}

	resp, handled, err := Handle("/new work", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /new")
	}
	if !strings.Contains(resp, "已存在") {
		t.Fatalf("response = %q, want duplicate message", resp)
	}
}

func TestHandleHelp(t *testing.T) {
	resp, handled, err := Handle("/help", "user", &fakeSessionManager{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /help")
	}
	for _, want := range []string{"/help", "/current", "/new", "/list", "/switch", "/rename", "/archive", "/clear", "/model", "/compact"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
}

func TestHelpTextIncludesDefaultCommands(t *testing.T) {
	resp := HelpText(DefaultPolicy())
	for _, want := range []string{"/help", "/current", "/new", "/list", "/switch", "/rename", "/archive", "/clear", "/model", "/compact"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
	if !strings.Contains(resp, "- `/help` - 查看命令帮助") {
		t.Fatalf("response = %q, want markdown command bullet", resp)
	}
}

func TestHelpTextWithToolsIncludesSummaries(t *testing.T) {
	resp := HelpTextWithTools(DefaultPolicy(), []ToolSummary{
		{Name: "feishu_docs_search", Description: "Search Feishu Docs and Wiki visible to the configured Feishu app.\nReturns links."},
		{Name: "feishu_docs_read", Description: "Read plain text from a Feishu docx document by token or URL."},
		{Name: "feishu_docs_search", Description: "duplicate"},
		{Name: "", Description: "missing name"},
	})
	for _, want := range []string{"## 可用工具", "- `feishu_docs_search` - Search Feishu Docs and Wiki visible to the configured Feishu app. Returns links.", "- `feishu_docs_read`"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
	if strings.Count(resp, "feishu_docs_search") != 1 {
		t.Fatalf("response = %q, want deduped feishu_docs_search", resp)
	}
}

func TestHandleClear(t *testing.T) {
	manager := &fakeSessionManager{}
	resp, handled, err := Handle("/clear", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /clear")
	}
	if !manager.clearCalled {
		t.Fatal("ClearSession was not called")
	}
	for _, want := range []string{"已清空当前会话", "session-1"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
}

func TestHandleClearRejectsArgs(t *testing.T) {
	manager := &fakeSessionManager{}
	resp, handled, err := Handle("/clear work", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /clear")
	}
	if manager.clearCalled {
		t.Fatal("ClearSession was called for /clear with args")
	}
	if resp != "用法：/clear" {
		t.Fatalf("response = %q, want usage", resp)
	}
}

func TestHandleWithPolicyDisabledCommand(t *testing.T) {
	manager := &fakeSessionManager{}
	resp, handled, err := HandleWithPolicy("/model", "user", manager, PolicyWithDisabled("/model"))
	if err != nil {
		t.Fatalf("HandleWithPolicy returned error: %v", err)
	}
	if !handled {
		t.Fatal("HandleWithPolicy did not handle disabled /model")
	}
	if !strings.Contains(resp, "暂不支持 /model") {
		t.Fatalf("response = %q, want unsupported command message", resp)
	}
}

func TestHelpTextUsesPolicy(t *testing.T) {
	resp := HelpText(PolicyWithDisabled("/model"))
	if strings.Contains(resp, "/model") {
		t.Fatalf("response = %q, want /model hidden", resp)
	}
	if !strings.Contains(resp, "/current") {
		t.Fatalf("response = %q, want other shared commands visible", resp)
	}
}

func TestClearUsesPolicy(t *testing.T) {
	manager := &fakeSessionManager{}
	resp := HelpText(PolicyWithDisabled("/clear"))
	if strings.Contains(resp, "/clear") {
		t.Fatalf("response = %q, want /clear hidden", resp)
	}

	resp, handled, err := HandleWithPolicy("/clear", "user", manager, PolicyWithDisabled("/clear"))
	if err != nil {
		t.Fatalf("HandleWithPolicy returned error: %v", err)
	}
	if !handled {
		t.Fatal("HandleWithPolicy did not handle disabled /clear")
	}
	if manager.clearCalled {
		t.Fatal("ClearSession was called for disabled /clear")
	}
	if !strings.Contains(resp, "暂不支持 /clear") {
		t.Fatalf("response = %q, want unsupported command message", resp)
	}
}

func TestHandleHelpUsesPolicy(t *testing.T) {
	resp, handled, err := HandleWithPolicy("/help", "user", &fakeSessionManager{}, PolicyWithDisabled("/model"))
	if err != nil {
		t.Fatalf("HandleWithPolicy returned error: %v", err)
	}
	if !handled {
		t.Fatal("HandleWithPolicy did not handle /help")
	}
	if strings.Contains(resp, "/model") {
		t.Fatalf("response = %q, want /model hidden", resp)
	}
	if !strings.Contains(resp, "/current") {
		t.Fatalf("response = %q, want other shared commands visible", resp)
	}
}

func TestHandleSwitchMissingSession(t *testing.T) {
	manager := &fakeSessionManager{
		switchErr: fmt.Errorf("%w: missing", store.ErrSessionNotFound),
	}

	resp, handled, err := Handle("/switch missing", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /switch")
	}
	if !strings.Contains(resp, "不存在") {
		t.Fatalf("response = %q, want not found message", resp)
	}
}

func TestHandleListSessions(t *testing.T) {
	manager := &fakeSessionManager{
		sessions: []store.Session{{ID: "1", UserID: "user", Name: "default", Current: true}},
	}

	resp, handled, err := Handle("/list", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /list")
	}
	if !strings.Contains(resp, "default") {
		t.Fatalf("response = %q, want session name", resp)
	}
}

func TestHandleCurrent(t *testing.T) {
	manager := &fakeSessionManager{currentModel: "gpt4o"}
	resp, handled, err := Handle("/current", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /current")
	}
	for _, want := range []string{"default", "gpt4o"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
	if manager.modelSession != "current" {
		t.Fatalf("model session = %q, want current", manager.modelSession)
	}
}

func TestHandleRenameDuplicateSession(t *testing.T) {
	manager := &fakeSessionManager{
		renameErr: fmt.Errorf("%w: work", store.ErrSessionExists),
	}

	resp, handled, err := Handle("/rename work", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /rename")
	}
	if !strings.Contains(resp, "已存在") {
		t.Fatalf("response = %q, want duplicate message", resp)
	}
}

func TestHandleArchiveCurrent(t *testing.T) {
	resp, handled, err := Handle("/archive", "user", &fakeSessionManager{})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /archive")
	}
	for _, want := range []string{"已归档", "next"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
}

func TestHandleModelUnknown(t *testing.T) {
	manager := &fakeSessionManager{
		setModelErr: fmt.Errorf("%w: missing", session.ErrModelNotFound),
	}

	resp, handled, err := Handle("/model missing", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /model")
	}
	if !strings.Contains(resp, "不存在") || !strings.Contains(resp, "deepseek") {
		t.Fatalf("response = %q, want unknown model message", resp)
	}
}

func TestHandleModelShowsCurrentAndAvailable(t *testing.T) {
	manager := &fakeSessionManager{currentModel: "gpt4o"}
	resp, handled, err := Handle("/model", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /model")
	}
	for _, want := range []string{"gpt4o", "deepseek"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response = %q, want %s", resp, want)
		}
	}
	if manager.modelSession != "current" {
		t.Fatalf("model session = %q, want current", manager.modelSession)
	}
}

func TestHandleModelSetsCurrentSession(t *testing.T) {
	manager := &fakeSessionManager{}
	resp, handled, err := Handle("/model gpt4o", "user", manager)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !handled {
		t.Fatal("Handle did not handle /model")
	}
	if !strings.Contains(resp, "gpt4o") {
		t.Fatalf("response = %q, want gpt4o", resp)
	}
	if manager.setSession != "current" {
		t.Fatalf("set model session = %q, want current", manager.setSession)
	}
}
