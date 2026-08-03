package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrFeishuChatFolderNotFound is returned when a folder is not bound to the requested chat.
	ErrFeishuChatFolderNotFound = errors.New("feishu chat folder not found")
	// ErrFeishuChatDocumentNotFound is returned when a document is not bound to the requested chat.
	ErrFeishuChatDocumentNotFound = errors.New("feishu chat document not found")
)

const (
	FeishuFolderShareStatePending   = "pending"
	FeishuFolderShareStateSucceeded = "succeeded"
	FeishuFolderShareStateFailed    = "failed"
)

// FeishuChatFolder binds one application-owned folder to one exact bot account and chat.
type FeishuChatFolder struct {
	AccountID         string
	ChatID            string
	FolderToken       string
	Name              string
	URL               string
	ParentFolderToken string
	Default           bool
	ShareMemberType   string
	ShareMemberID     string
	ShareState        string
	CreateRequestID   string
	CreatedByOpenID   string
	CreatedByUserID   string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// FeishuChatDocument binds one document token to the chat-scoped folder where it was discovered or created.
type FeishuChatDocument struct {
	AccountID       string
	ChatID          string
	DocumentToken   string
	FolderToken     string
	Title           string
	URL             string
	SourceRequestID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SaveFeishuChatFolder stores a newly created application-owned folder. The
// first folder in a chat becomes the default automatically.
func (s *Store) SaveFeishuChatFolder(folder FeishuChatFolder) (FeishuChatFolder, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatFolder{}, err
	}
	folder = normalizeFeishuChatFolder(folder)
	if err := validateFeishuChatFolder(folder); err != nil {
		return FeishuChatFolder{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return FeishuChatFolder{}, fmt.Errorf("begin save feishu chat folder: %w", err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM feishu_chat_folders WHERE account_id=? AND chat_id=?`,
		folder.AccountID, folder.ChatID,
	).Scan(&count); err != nil {
		return FeishuChatFolder{}, fmt.Errorf("count feishu chat folders: %w", err)
	}
	if count == 0 {
		folder.Default = true
	}
	if folder.Default {
		if _, err := tx.Exec(
			`UPDATE feishu_chat_folders SET is_default=0, updated_at_ms=? WHERE account_id=? AND chat_id=? AND is_default=1`,
			folder.UpdatedAt.UnixMilli(), folder.AccountID, folder.ChatID,
		); err != nil {
			return FeishuChatFolder{}, fmt.Errorf("clear default feishu chat folder: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO feishu_chat_folders (
			account_id, chat_id, folder_token, name, url, parent_folder_token,
			is_default, share_member_type, share_member_id, share_state,
			create_request_id, created_by_open_id, created_by_user_id,
			created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		folder.AccountID,
		folder.ChatID,
		folder.FolderToken,
		folder.Name,
		folder.URL,
		folder.ParentFolderToken,
		boolToInt(folder.Default),
		folder.ShareMemberType,
		folder.ShareMemberID,
		folder.ShareState,
		folder.CreateRequestID,
		folder.CreatedByOpenID,
		folder.CreatedByUserID,
		folder.CreatedAt.UnixMilli(),
		folder.UpdatedAt.UnixMilli(),
	); err != nil {
		return FeishuChatFolder{}, fmt.Errorf("save feishu chat folder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FeishuChatFolder{}, fmt.Errorf("commit feishu chat folder: %w", err)
	}
	return folder, nil
}

// UpdateFeishuChatFolderShareState records whether full-access sharing completed.
func (s *Store) UpdateFeishuChatFolderShareState(accountID, chatID, folderToken, state string, now time.Time) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	chatID = strings.TrimSpace(chatID)
	folderToken = strings.TrimSpace(folderToken)
	state = strings.TrimSpace(state)
	if accountID == "" || chatID == "" || folderToken == "" || !validFeishuFolderShareState(state) {
		return fmt.Errorf("feishu folder account_id, chat_id, folder_token, and valid share state are required")
	}
	now = normalizedWorkflowTime(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(
		`UPDATE feishu_chat_folders SET share_state=?, updated_at_ms=?
		 WHERE account_id=? AND chat_id=? AND folder_token=?`,
		state, now.UnixMilli(), accountID, chatID, folderToken,
	)
	if err != nil {
		return fmt.Errorf("update feishu chat folder share state: %w", err)
	}
	return requireOneFeishuFolderRow(result)
}

// GetFeishuChatFolder returns a folder only when it is bound to the exact chat.
func (s *Store) GetFeishuChatFolder(accountID, chatID, folderToken string) (FeishuChatFolder, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatFolder{}, err
	}
	return scanFeishuChatFolder(s.db.QueryRow(
		feishuChatFolderSelect+` WHERE account_id=? AND chat_id=? AND folder_token=?`,
		strings.TrimSpace(accountID), strings.TrimSpace(chatID), strings.TrimSpace(folderToken),
	))
}

// GetFeishuChatFolderByRequest returns the folder created by one root request in the exact chat.
func (s *Store) GetFeishuChatFolderByRequest(accountID, chatID, requestID string) (FeishuChatFolder, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatFolder{}, err
	}
	return scanFeishuChatFolder(s.db.QueryRow(
		feishuChatFolderSelect+` WHERE account_id=? AND chat_id=? AND create_request_id=?`,
		strings.TrimSpace(accountID), strings.TrimSpace(chatID), strings.TrimSpace(requestID),
	))
}

// DefaultFeishuChatFolder returns the default folder for one exact chat.
func (s *Store) DefaultFeishuChatFolder(accountID, chatID string) (FeishuChatFolder, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatFolder{}, err
	}
	return scanFeishuChatFolder(s.db.QueryRow(
		feishuChatFolderSelect+` WHERE account_id=? AND chat_id=? AND is_default=1`,
		strings.TrimSpace(accountID), strings.TrimSpace(chatID),
	))
}

// ListFeishuChatFolders returns all folders bound to one chat, default first.
func (s *Store) ListFeishuChatFolders(accountID, chatID string) ([]FeishuChatFolder, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		feishuChatFolderSelect+` WHERE account_id=? AND chat_id=? ORDER BY is_default DESC, created_at_ms ASC, folder_token ASC`,
		strings.TrimSpace(accountID), strings.TrimSpace(chatID),
	)
	if err != nil {
		return nil, fmt.Errorf("list feishu chat folders: %w", err)
	}
	defer rows.Close()
	folders := []FeishuChatFolder{}
	for rows.Next() {
		folder, err := scanFeishuChatFolder(rows)
		if err != nil {
			return nil, err
		}
		folders = append(folders, folder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list feishu chat folders: %w", err)
	}
	return folders, nil
}

// SaveFeishuChatDocument records a document as accessible only in one exact chat.
func (s *Store) SaveFeishuChatDocument(document FeishuChatDocument) (FeishuChatDocument, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatDocument{}, err
	}
	document = normalizeFeishuChatDocument(document)
	if err := validateFeishuChatDocument(document); err != nil {
		return FeishuChatDocument{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO feishu_chat_documents (
			account_id, chat_id, document_token, folder_token, title, url,
			source_request_id, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, chat_id, document_token) DO UPDATE SET
			folder_token=excluded.folder_token,
			title=excluded.title,
			url=excluded.url,
			source_request_id=CASE
				WHEN excluded.source_request_id='' THEN feishu_chat_documents.source_request_id
				ELSE excluded.source_request_id
			END,
			updated_at_ms=excluded.updated_at_ms`,
		document.AccountID,
		document.ChatID,
		document.DocumentToken,
		document.FolderToken,
		document.Title,
		document.URL,
		document.SourceRequestID,
		document.CreatedAt.UnixMilli(),
		document.UpdatedAt.UnixMilli(),
	)
	if err != nil {
		return FeishuChatDocument{}, fmt.Errorf("save feishu chat document: %w", err)
	}
	return document, nil
}

// GetFeishuChatDocument returns a document only when it is bound to the exact chat.
func (s *Store) GetFeishuChatDocument(accountID, chatID, documentToken string) (FeishuChatDocument, error) {
	if err := s.requireFeishuDocsStore(); err != nil {
		return FeishuChatDocument{}, err
	}
	return scanFeishuChatDocument(s.db.QueryRow(
		`SELECT account_id, chat_id, document_token, folder_token, title, url,
		 source_request_id, created_at_ms, updated_at_ms
		 FROM feishu_chat_documents WHERE account_id=? AND chat_id=? AND document_token=?`,
		strings.TrimSpace(accountID), strings.TrimSpace(chatID), strings.TrimSpace(documentToken),
	))
}

// DeleteFeishuDocsData removes all Feishu document/resource metadata and
// document-related workflow requests for one bot account.
func (s *Store) DeleteFeishuDocsData(accountID string) error {
	if err := s.requireFeishuDocsStore(); err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete feishu docs data: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM feishu_chat_documents WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu chat documents: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM feishu_chat_folders WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu chat folders: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM feishu_resource_grants WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu resource grants: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM feishu_user_oauth_credentials WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu user oauth credentials: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM feishu_resource_access_requests WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu resource access requests: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM feishu_bot_resources WHERE account_id=?`, accountID); err != nil {
		return fmt.Errorf("delete feishu bot resources: %w", err)
	}
	if err := deleteWorkflowRuntimeData(
		tx,
		accountID,
		WorkflowRequestKindFeishuFolderCreate,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuResourceAccess,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM workflow_requests WHERE account_id=? AND kind IN (?, ?, ?, ?)`,
		accountID,
		WorkflowRequestKindFeishuFolderCreate,
		WorkflowRequestKindFeishuDocsCreate,
		WorkflowRequestKindFeishuDocsAppend,
		WorkflowRequestKindFeishuResourceAccess,
	); err != nil {
		return fmt.Errorf("delete feishu docs workflow requests: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete feishu docs data: %w", err)
	}
	return nil
}

const feishuChatFolderSelect = `SELECT account_id, chat_id, folder_token, name, url, parent_folder_token,
 is_default, share_member_type, share_member_id, share_state, create_request_id,
 created_by_open_id, created_by_user_id, created_at_ms, updated_at_ms
 FROM feishu_chat_folders`

type feishuDocsScanner interface {
	Scan(dest ...any) error
}

func scanFeishuChatFolder(row feishuDocsScanner) (FeishuChatFolder, error) {
	var folder FeishuChatFolder
	var isDefault int
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&folder.AccountID,
		&folder.ChatID,
		&folder.FolderToken,
		&folder.Name,
		&folder.URL,
		&folder.ParentFolderToken,
		&isDefault,
		&folder.ShareMemberType,
		&folder.ShareMemberID,
		&folder.ShareState,
		&folder.CreateRequestID,
		&folder.CreatedByOpenID,
		&folder.CreatedByUserID,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuChatFolder{}, ErrFeishuChatFolderNotFound
		}
		return FeishuChatFolder{}, fmt.Errorf("get feishu chat folder: %w", err)
	}
	folder.Default = isDefault != 0
	folder.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	folder.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return folder, nil
}

func scanFeishuChatDocument(row feishuDocsScanner) (FeishuChatDocument, error) {
	var document FeishuChatDocument
	var createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&document.AccountID,
		&document.ChatID,
		&document.DocumentToken,
		&document.FolderToken,
		&document.Title,
		&document.URL,
		&document.SourceRequestID,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FeishuChatDocument{}, ErrFeishuChatDocumentNotFound
		}
		return FeishuChatDocument{}, fmt.Errorf("get feishu chat document: %w", err)
	}
	document.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	document.UpdatedAt = time.UnixMilli(updatedAtMS).UTC()
	return document, nil
}

func normalizeFeishuChatFolder(folder FeishuChatFolder) FeishuChatFolder {
	folder.AccountID = strings.TrimSpace(folder.AccountID)
	folder.ChatID = strings.TrimSpace(folder.ChatID)
	folder.FolderToken = strings.TrimSpace(folder.FolderToken)
	folder.Name = strings.TrimSpace(folder.Name)
	folder.URL = strings.TrimSpace(folder.URL)
	folder.ParentFolderToken = strings.TrimSpace(folder.ParentFolderToken)
	folder.ShareMemberType = strings.TrimSpace(folder.ShareMemberType)
	folder.ShareMemberID = strings.TrimSpace(folder.ShareMemberID)
	folder.ShareState = strings.TrimSpace(folder.ShareState)
	folder.CreateRequestID = strings.TrimSpace(folder.CreateRequestID)
	folder.CreatedByOpenID = strings.TrimSpace(folder.CreatedByOpenID)
	folder.CreatedByUserID = strings.TrimSpace(folder.CreatedByUserID)
	folder.CreatedAt = normalizedWorkflowTime(folder.CreatedAt)
	folder.UpdatedAt = folder.CreatedAt
	return folder
}

func validateFeishuChatFolder(folder FeishuChatFolder) error {
	if folder.AccountID == "" || folder.ChatID == "" || folder.FolderToken == "" || folder.Name == "" ||
		folder.ShareMemberType == "" || folder.ShareMemberID == "" || folder.CreateRequestID == "" ||
		!validFeishuFolderShareState(folder.ShareState) {
		return fmt.Errorf("feishu chat folder account, chat, token, name, share target/state, and create request are required")
	}
	return nil
}

func validFeishuFolderShareState(state string) bool {
	return state == FeishuFolderShareStatePending || state == FeishuFolderShareStateSucceeded || state == FeishuFolderShareStateFailed
}

func normalizeFeishuChatDocument(document FeishuChatDocument) FeishuChatDocument {
	document.AccountID = strings.TrimSpace(document.AccountID)
	document.ChatID = strings.TrimSpace(document.ChatID)
	document.DocumentToken = strings.TrimSpace(document.DocumentToken)
	document.FolderToken = strings.TrimSpace(document.FolderToken)
	document.Title = strings.TrimSpace(document.Title)
	document.URL = strings.TrimSpace(document.URL)
	document.SourceRequestID = strings.TrimSpace(document.SourceRequestID)
	document.CreatedAt = normalizedWorkflowTime(document.CreatedAt)
	document.UpdatedAt = document.CreatedAt
	return document
}

func validateFeishuChatDocument(document FeishuChatDocument) error {
	if document.AccountID == "" || document.ChatID == "" || document.DocumentToken == "" || document.FolderToken == "" {
		return fmt.Errorf("feishu chat document account, chat, document token, and folder token are required")
	}
	return nil
}

func requireOneFeishuFolderRow(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect feishu chat folder update: %w", err)
	}
	if count != 1 {
		return ErrFeishuChatFolderNotFound
	}
	return nil
}

func (s *Store) requireFeishuDocsStore() error {
	if s == nil || s.platformID != PlatformFeishu {
		platformID := ""
		if s != nil {
			platformID = s.platformID
		}
		return fmt.Errorf("feishu docs data requires %q store, got %q", PlatformFeishu, platformID)
	}
	return nil
}
