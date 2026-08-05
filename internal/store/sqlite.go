package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	_ "modernc.org/sqlite"

	"lingobridge/internal/config"
)

var (
	// ErrSessionExists is returned when a user already has a session with the requested name.
	ErrSessionExists = errors.New("session already exists")
	// ErrSessionNotFound is returned when a named session cannot be found for a user.
	ErrSessionNotFound = errors.New("session not found")
)

// Store provides SQLite-backed metadata storage.
type Store struct {
	db         *sql.DB
	platformID string
	dataDir    string
	mu         sync.Mutex // serializes writes across goroutines
}

const (
	// PlatformWeChat identifies accounts backed by the WeChat bot API.
	PlatformWeChat = "wechat"
	// PlatformFeishu identifies accounts backed by the Feishu Open Platform.
	PlatformFeishu = "feishu"
	// PlatformGitHub identifies accounts backed by GitHub App PR review polling.
	PlatformGitHub = "github"
)

// Account represents a bot account saved during login or configuration.
type Account struct {
	ID              string
	Name            string
	Platform        string
	Token           string
	BaseURL         string
	UserID          string
	CredentialsJSON string
	Enabled         bool
}

func (a *Account) applyDefaults() {
	if a.CredentialsJSON == "" {
		a.CredentialsJSON = "{}"
	}
}

// Session represents a named conversation session for a user.
type Session struct {
	ID        string
	UserID    string
	Name      string
	ModelName string
	Archived  bool
	Current   bool
	CreatedAt string
}

// ArchiveResult describes the result of archiving a session.
type ArchiveResult struct {
	Archived       Session
	Current        *Session
	CurrentChanged bool
}

// Open creates or opens the SQLite database for one isolated platform.
func Open(platformID string) (*Store, error) {
	dataDir, err := config.EnsurePlatformDataDir(platformID)
	if err != nil {
		return nil, err
	}

	dbPath, err := config.PlatformDBPath(platformID)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Enable WAL mode for concurrent reads, and busy timeout for concurrent writes.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	s := &Store{db: db, platformID: platformID, dataDir: dataDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod sqlite: %w", err)
	}

	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// PlatformID returns the platform this store is allowed to access.
func (s *Store) PlatformID() string {
	return s.platformID
}

// DataDir returns this store's isolated data directory.
func (s *Store) DataDir() string {
	return s.dataDir
}
