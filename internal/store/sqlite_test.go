package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lingobridge/internal/config"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	st, err := Open(PlatformWeChat)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return st
}

func TestCreateSessionDuplicateName(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateSession("user", "work"); err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}
	_, err := st.CreateSession("user", "work")
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("CreateSession duplicate error = %v, want ErrSessionExists", err)
	}
}

func TestSwitchSessionNotFound(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateSession("user", "work"); err != nil {
		t.Fatalf("CreateSession returned error: %v", err)
	}
	_, err := st.SwitchSession("user", "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SwitchSession error = %v, want ErrSessionNotFound", err)
	}
}

func TestCurrentSessionUsesUserPreference(t *testing.T) {
	st := openTestStore(t)

	work, err := st.CreateSession("user", "work")
	if err != nil {
		t.Fatalf("CreateSession work returned error: %v", err)
	}
	play, err := st.CreateSession("user", "play")
	if err != nil {
		t.Fatalf("CreateSession play returned error: %v", err)
	}
	if !play.Current {
		t.Fatal("new session is not current")
	}

	current, err := st.SwitchSession("user", "work")
	if err != nil {
		t.Fatalf("SwitchSession returned error: %v", err)
	}
	if current.ID != work.ID || !current.Current {
		t.Fatalf("current session = %#v, want work", current)
	}

	sessions, err := st.ListSessions("user")
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	currentCount := 0
	for _, sess := range sessions {
		if sess.Current {
			currentCount++
			if sess.ID != work.ID {
				t.Fatalf("current session = %s, want %s", sess.ID, work.ID)
			}
		}
	}
	if currentCount != 1 {
		t.Fatalf("current session count = %d, want 1", currentCount)
	}
}

func TestArchiveCurrentSessionFallsBack(t *testing.T) {
	st := openTestStore(t)

	if _, err := st.CreateSession("user", "work"); err != nil {
		t.Fatalf("CreateSession work returned error: %v", err)
	}
	if _, err := st.CreateSession("user", "play"); err != nil {
		t.Fatalf("CreateSession play returned error: %v", err)
	}
	if _, err := st.ArchiveSession("user", "play"); err != nil {
		t.Fatalf("ArchiveSession returned error: %v", err)
	}

	current, err := st.GetCurrentSession("user")
	if err != nil {
		t.Fatalf("GetCurrentSession returned error: %v", err)
	}
	if current.Name != "work" {
		t.Fatalf("current session = %s, want work", current.Name)
	}

	sessions, err := st.ListSessions("user")
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	for _, sess := range sessions {
		if sess.Name == "play" {
			t.Fatalf("archived session appeared in list: %#v", sess)
		}
	}
}

func TestResetUnavailableSessionModels(t *testing.T) {
	st := openTestStore(t)

	user1, err := st.CreateSession("user1", "work")
	if err != nil {
		t.Fatalf("CreateSession user1 returned error: %v", err)
	}
	user2, err := st.CreateSession("user2", "work")
	if err != nil {
		t.Fatalf("CreateSession user2 returned error: %v", err)
	}
	if err := st.SetSessionModelName("user1", user1.ID, "old"); err != nil {
		t.Fatalf("SetSessionModelName user1 returned error: %v", err)
	}
	if err := st.SetSessionModelName("user2", user2.ID, "deepseek"); err != nil {
		t.Fatalf("SetSessionModelName user2 returned error: %v", err)
	}

	count, err := st.ResetUnavailableSessionModels("deepseek", []string{"deepseek", "gpt4o"})
	if err != nil {
		t.Fatalf("ResetUnavailableSessionModels returned error: %v", err)
	}
	if count != 1 {
		t.Fatalf("reset count = %d, want 1", count)
	}

	model, err := st.GetSessionModelName("user1", user1.ID)
	if err != nil {
		t.Fatalf("GetSessionModelName user1 returned error: %v", err)
	}
	if model != "deepseek" {
		t.Fatalf("user1 model = %q, want deepseek", model)
	}
	model, err = st.GetSessionModelName("user2", user2.ID)
	if err != nil {
		t.Fatalf("GetSessionModelName user2 returned error: %v", err)
	}
	if model != "deepseek" {
		t.Fatalf("user2 model = %q, want deepseek", model)
	}
}

func TestOpenMigratesUserModelPreferenceToExistingSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dataDir, err := config.EnsurePlatformDataDir(PlatformWeChat)
	if err != nil {
		t.Fatalf("EnsurePlatformDataDir returned error: %v", err)
	}
	dbPath := filepath.Join(dataDir, "lingobridge.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT 'default',
		archived INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("create legacy sessions returned error: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE user_preferences (
		user_id TEXT PRIMARY KEY,
		current_session_id TEXT NOT NULL DEFAULT '',
		model_name TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatalf("create legacy preferences returned error: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, user_id, name, archived) VALUES
		('work', 'user', 'work', 0),
		('play', 'user', 'play', 0)`); err != nil {
		t.Fatalf("insert legacy sessions returned error: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO user_preferences (user_id, current_session_id, model_name)
		VALUES ('user', 'work', 'gpt4o')`); err != nil {
		t.Fatalf("insert legacy preference returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close returned error: %v", err)
	}

	st, err := Open(PlatformWeChat)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	for _, sessionID := range []string{"work", "play"} {
		model, err := st.GetSessionModelName("user", sessionID)
		if err != nil {
			t.Fatalf("GetSessionModelName %s returned error: %v", sessionID, err)
		}
		if model != "gpt4o" {
			t.Fatalf("session %s model = %q, want gpt4o", sessionID, model)
		}
	}
	hasLegacyColumn, err := st.tableHasColumn("user_preferences", "model_name")
	if err != nil {
		t.Fatalf("tableHasColumn returned error: %v", err)
	}
	if hasLegacyColumn {
		t.Fatal("legacy user_preferences.model_name still exists after migration")
	}
}

func TestOpenAddsWorkflowContinuationChatTypeColumn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dataDir, err := config.EnsurePlatformDataDir(PlatformFeishu)
	if err != nil {
		t.Fatalf("EnsurePlatformDataDir returned error: %v", err)
	}
	dbPath := filepath.Join(dataDir, "lingobridge.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE workflow_continuations (
		request_id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		platform TEXT NOT NULL,
		user_key TEXT NOT NULL,
		session_id TEXT NOT NULL,
		chat_id TEXT NOT NULL DEFAULT '',
		source_message_id TEXT NOT NULL DEFAULT '',
		actor_open_id TEXT NOT NULL DEFAULT '',
		actor_user_id TEXT NOT NULL DEFAULT '',
		origin_revision INTEGER NOT NULL,
		committed_revision INTEGER NOT NULL DEFAULT -1,
		origin_turn_id TEXT NOT NULL,
		tool_call_id TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		state TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		available_at_ms INTEGER NOT NULL,
		lease_token TEXT NOT NULL DEFAULT '',
		lease_expires_at_ms INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		t.Fatalf("create legacy workflow continuations returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close returned error: %v", err)
	}

	st, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	hasColumn, err := st.tableHasColumn("workflow_continuations", "chat_is_group")
	if err != nil {
		t.Fatalf("tableHasColumn returned error: %v", err)
	}
	if !hasColumn {
		t.Fatal("workflow_continuations.chat_is_group was not added")
	}
}

func TestSaveAccountRequiresMatchingPlatform(t *testing.T) {
	st := openTestStore(t)

	err := st.SaveAccount(Account{ID: "a1", Name: "bot", Token: "token", BaseURL: "https://wechat.test", Enabled: true})
	if err == nil {
		t.Fatal("SaveAccount returned nil error, want platform mismatch")
	}
	if err := st.SaveAccount(Account{ID: "a1", Name: "bot", Platform: PlatformWeChat, Token: "token", BaseURL: "https://wechat.test", Enabled: true}); err != nil {
		t.Fatalf("SaveAccount returned error: %v", err)
	}
	got, err := st.GetAccount("a1")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if got.Platform != PlatformWeChat {
		t.Fatalf("platform = %q, want %q", got.Platform, PlatformWeChat)
	}
	if got.CredentialsJSON != "{}" {
		t.Fatalf("credentials_json = %q, want {}", got.CredentialsJSON)
	}
}

func TestSaveFeishuAccountMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := Open(PlatformFeishu)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	account := Account{
		ID:              "feishu:cli_xxx",
		Name:            "fsbot",
		Platform:        PlatformFeishu,
		BaseURL:         "https://open.feishu.cn",
		UserID:          "cli_xxx",
		CredentialsJSON: "{}",
		Enabled:         true,
	}
	if err := st.SaveAccount(account); err != nil {
		t.Fatalf("SaveAccount returned error: %v", err)
	}
	got, err := st.GetAccount(account.ID)
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if got.Platform != PlatformFeishu || got.CredentialsJSON != "{}" || got.UserID != account.UserID {
		t.Fatalf("account = %#v, want feishu metadata preserved", got)
	}
}

func TestOpenUsesPlatformDatabaseNotLegacyGlobalDatabase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir returned error: %v", err)
	}
	legacyDataDir := filepath.Join(configDir, "data")
	if err := os.MkdirAll(legacyDataDir, 0700); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(legacyDataDir, "lingobridge.db"))
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE accounts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		token TEXT NOT NULL,
		base_url TEXT NOT NULL DEFAULT 'https://ilinkai.weixin.qq.com',
		user_id TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1
	)`)
	if err != nil {
		t.Fatalf("create old accounts returned error: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (id, name, token, base_url, user_id, enabled) VALUES ('a1', 'bot', 'token', 'base', 'user', 1)`); err != nil {
		t.Fatalf("insert old account returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close returned error: %v", err)
	}

	st, err := Open(PlatformWeChat)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts returned error: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("platform store read legacy global accounts: %#v", accounts)
	}
}

func TestOpenDoesNotMigrateLegacyActiveSessionSchema(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dataDir, err := config.EnsurePlatformDataDir(PlatformWeChat)
	if err != nil {
		t.Fatalf("EnsurePlatformDataDir returned error: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "lingobridge.db"))
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT 'default',
		active INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		t.Fatalf("create old sessions returned error: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (id, user_id, name, active, created_at) VALUES
		('old-current', 'user', 'current', 1, '2026-01-02 00:00:00'),
		('old-other', 'user', 'other', 0, '2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatalf("insert old sessions returned error: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close returned error: %v", err)
	}

	st, err := Open(PlatformWeChat)
	if err == nil {
		st.Close()
		t.Fatal("Open returned nil error, want legacy schema failure")
	}
}
