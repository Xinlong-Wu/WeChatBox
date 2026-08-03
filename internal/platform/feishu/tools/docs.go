package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdocx "github.com/larksuite/oapi-sdk-go/v3/service/docx/v1"
	larksearch "github.com/larksuite/oapi-sdk-go/v3/service/search/v2"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const (
	searchToolName = "feishu_docs_search"
	readToolName   = "feishu_docs_read"
	createToolName = "feishu_docs_create"
	appendToolName = "feishu_docs_append"
	docxTextBlock  = 2
	maxDocxTitle   = 800
)

type docsTool struct {
	name           string
	spec           tooltypes.Spec
	client         *lark.Client
	store          *store.Store
	accountID      string
	cfg            Config
	approvals      OperationApprovalService
	resourceAccess ResourceAccessController
	now            func() time.Time
}

// NewDocsTools returns Feishu document tools for tool-capable LLM providers.
func NewDocsTools(client *lark.Client, st *store.Store, accountID string, cfg Config, approvals OperationApprovalService, resourceAccess ResourceAccessController) []tooltypes.Tool {
	cfg = NormalizeConfig(cfg)
	accountID = strings.TrimSpace(accountID)
	if client == nil || st == nil || st.PlatformID() != store.PlatformFeishu || accountID == "" || !cfg.Docs.Enabled {
		return nil
	}
	tools := []tooltypes.Tool{
		docsTool{name: searchToolName, spec: docsSearchSpec(), client: client, store: st, accountID: accountID, cfg: cfg, now: time.Now},
		docsTool{name: readToolName, spec: docsReadSpec(), client: client, store: st, accountID: accountID, cfg: cfg, resourceAccess: resourceAccess, now: time.Now},
	}
	if cfg.Docs.AllowWrite {
		if approvals != nil && resourceAccess != nil {
			tools = append(tools, docsTool{name: createToolName, spec: docsCreateSpec(), client: client, store: st, accountID: accountID, cfg: cfg, approvals: approvals, resourceAccess: resourceAccess, now: time.Now})
			tools = append(tools, docsTool{name: appendToolName, spec: docsAppendSpec(), client: client, store: st, accountID: accountID, cfg: cfg, approvals: approvals, resourceAccess: resourceAccess, now: time.Now})
		}
	}
	return tools
}

func (t docsTool) Spec() tooltypes.Spec {
	return t.spec
}

func (t docsTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	var content string
	var pendingWorkflowID string
	var err error
	switch t.name {
	case searchToolName:
		content, err = t.search(ctx, call.Arguments)
	case readToolName:
		content, err = t.read(ctx, call.Arguments)
	case createToolName:
		content, pendingWorkflowID, err = t.create(ctx, call.Arguments)
	case appendToolName:
		content, pendingWorkflowID, err = t.append(ctx, call.Arguments)
	default:
		err = fmt.Errorf("unsupported feishu docs tool %q", t.name)
	}
	return tooltypes.Result{
		CallID:            call.ID,
		Name:              t.name,
		Content:           contentOrError(content, err),
		IsError:           err != nil,
		PendingWorkflowID: pendingWorkflowID,
	}
}

func contentOrError(content string, err error) string {
	if err != nil {
		if structured, ok := ResourceAuthorizationRequiredContent(err); ok {
			return structured
		}
		return err.Error()
	}
	return content
}

type searchArgs struct {
	Query    string `json:"query"`
	MaxItems int    `json:"max_items,omitempty"`
}

type readArgs struct {
	Token string `json:"token,omitempty"`
	URL   string `json:"url,omitempty"`
	Type  string `json:"type,omitempty"`
}

type createArgs struct {
	Title       string `json:"title"`
	Content     string `json:"content,omitempty"`
	FolderToken string `json:"folder_token,omitempty"`
}

type approvedCreatePayload struct {
	createArgs
	ChatID      string `json:"chat_id"`
	ActorOpenID string `json:"actor_open_id,omitempty"`
	ActorUserID string `json:"actor_user_id,omitempty"`
}

type approvedAppendPayload struct {
	DocumentToken string `json:"document_token"`
	Content       string `json:"content"`
	FolderToken   string `json:"folder_token,omitempty"`
	ChatID        string `json:"chat_id"`
	ActorOpenID   string `json:"actor_open_id,omitempty"`
	ActorUserID   string `json:"actor_user_id,omitempty"`
}

type appendArgs struct {
	Token       string `json:"token,omitempty"`
	URL         string `json:"url,omitempty"`
	Content     string `json:"content"`
	FolderToken string `json:"folder_token,omitempty"`
}

type searchOutput struct {
	Query    string         `json:"query"`
	Results  []searchResult `json:"results"`
	Warnings []string       `json:"warnings,omitempty"`
}

type searchResult struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Type    string `json:"type,omitempty"`
	URL     string `json:"url,omitempty"`
	Token   string `json:"token,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

type readOutput struct {
	Token     string `json:"token"`
	Type      string `json:"type"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

type writeOutput struct {
	RequestID  string `json:"request_id,omitempty"`
	DocumentID string `json:"document_id"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	Appended   bool   `json:"appended,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type pendingApprovalOutput struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	ExpiresAt string `json:"expires_at"`
	Message   string `json:"message"`
}

type folderRequiredOutput struct {
	Status        string `json:"status"`
	RequiredTool  string `json:"required_tool"`
	Message       string `json:"message"`
	CreateRequest string `json:"create_request_id,omitempty"`
}

func (t docsTool) search(ctx context.Context, raw json.RawMessage) (string, error) {
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	_, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return "", err
	}
	limit := args.MaxItems
	if limit <= 0 || limit > t.cfg.MaxResults {
		limit = t.cfg.MaxResults
	}
	if limit > 20 {
		limit = 20
	}
	folders, err := t.store.ListFeishuChatFolders(t.accountID, chat.ChatID)
	if err != nil {
		return "", fmt.Errorf("list current feishu chat folders: %w", err)
	}
	available := availableFeishuFolders(folders)
	if len(available) == 0 {
		return marshalToolOutput(folderRequiredForFolders(folders))
	}

	started := time.Now()
	out := searchOutput{Query: args.Query, Results: []searchResult{}}
	seen := map[string]struct{}{}
	succeeded := 0
	var firstSearchErr error
	for _, folder := range available {
		results, searchErr := t.searchFolder(ctx, args.Query, limit, folder)
		if searchErr != nil {
			if firstSearchErr == nil {
				firstSearchErr = searchErr
			}
			out.Warnings = append(out.Warnings, fmt.Sprintf("文件夹 %q 搜索失败。", folderDisplayName(folder)))
			feishuToolsLog.Warn(ctx, "search feishu chat folder failed account=%s chat=%s folder_ref=%s: %v", t.accountID, chat.ChatID, hashString(folder.FolderToken), searchErr)
			continue
		}
		succeeded++
		mappingWarningAdded := false
		for _, result := range results {
			if isWikiSearchResult(result) {
				continue
			}
			if result.Token != "" {
				if _, saveErr := t.store.SaveFeishuChatDocument(store.FeishuChatDocument{
					AccountID:     t.accountID,
					ChatID:        chat.ChatID,
					DocumentToken: result.Token,
					FolderToken:   folder.FolderToken,
					Title:         result.Title,
					URL:           result.URL,
					CreatedAt:     t.currentTime(),
				}); saveErr != nil {
					if !mappingWarningAdded {
						out.Warnings = append(out.Warnings, fmt.Sprintf("文件夹 %q 的部分搜索结果未能保存为当前对话可访问文档。", folderDisplayName(folder)))
						mappingWarningAdded = true
					}
					feishuToolsLog.Warn(ctx, "persist feishu search result failed account=%s chat=%s folder_ref=%s document_ref=%s: %v", t.accountID, chat.ChatID, hashString(folder.FolderToken), hashString(result.Token), saveErr)
				}
			}
			key := searchResultKey(result)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out.Results = append(out.Results, result)
		}
	}
	if succeeded == 0 {
		return "", fmt.Errorf("search current Feishu chat folders: %w", firstSearchErr)
	}
	if len(out.Results) > limit {
		out.Results = out.Results[:limit]
	}
	feishuToolsLog.Debug(ctx, "searched feishu chat documents account=%s chat=%s folders=%d succeeded=%d results=%d warnings=%d query_chars=%d duration_ms=%d",
		t.accountID, chat.ChatID, len(available), succeeded, len(out.Results), len(out.Warnings), utf8.RuneCountInString(args.Query), time.Since(started).Milliseconds())
	return marshalToolOutput(out)
}

func (t docsTool) searchFolder(ctx context.Context, query string, limit int, folder store.FeishuChatFolder) ([]searchResult, error) {
	req := larksearch.NewSearchDocWikiReqBuilder().
		Body(larksearch.NewSearchDocWikiReqBodyBuilder().
			Query(query).
			PageSize(limit).
			DocFilter(larksearch.NewDocFilterBuilder().FolderTokens([]string{folder.FolderToken}).Build()).
			Build()).
		Build()
	resp, err := t.client.Search.DocWiki.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search feishu docs: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("search feishu docs: empty response")
	}
	if !resp.Success() {
		return nil, fmt.Errorf("search feishu docs code=%d msg=%s", resp.Code, resp.Msg)
	}
	results := []searchResult{}
	if resp.Data == nil {
		return results, nil
	}
	for _, unit := range resp.Data.ResUnits {
		if unit == nil {
			continue
		}
		result := searchResult{
			Title:   stripSearchHighlight(deref(unit.TitleHighlighted)),
			Summary: stripSearchHighlight(deref(unit.SummaryHighlighted)),
			Type:    strings.TrimSpace(deref(unit.EntityType)),
		}
		if unit.ResultMeta != nil {
			result.URL = strings.TrimSpace(deref(unit.ResultMeta.Url))
			result.Token = strings.TrimSpace(deref(unit.ResultMeta.Token))
			result.Owner = strings.TrimSpace(deref(unit.ResultMeta.OwnerName))
			if result.Type == "" {
				result.Type = strings.TrimSpace(deref(unit.ResultMeta.DocTypes))
			}
		}
		results = append(results, result)
	}
	return results, nil
}

func (t docsTool) read(ctx context.Context, raw json.RawMessage) (string, error) {
	var args readArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	ref, err := parseDocRef(args.Token, args.URL, args.Type)
	if err != nil {
		return "", err
	}
	if ref.Kind != "docx" {
		return "", fmt.Errorf("reading %s documents is not supported yet; provide a docx token or URL", ref.Kind)
	}
	_, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return "", err
	}
	if _, _, err := t.authorizeDocumentAccess(ctx, chat, ref.Token, ResourcePermissionRead); err != nil {
		return "", err
	}
	req := larkdocx.NewRawContentDocumentReqBuilder().
		DocumentId(ref.Token).
		Lang(larkdocx.LangZH).
		Build()
	resp, err := t.client.Docx.Document.RawContent(ctx, req)
	if err != nil {
		return "", fmt.Errorf("read feishu document: %w", err)
	}
	if resp == nil || !resp.Success() {
		return "", fmt.Errorf("read feishu document code=%d msg=%s", resp.Code, resp.Msg)
	}
	content := ""
	if resp.Data != nil {
		content = deref(resp.Data.Content)
	}
	content, truncated := truncateRunes(content, t.cfg.MaxChars)
	return marshalToolOutput(readOutput{Token: ref.Token, Type: ref.Kind, Content: content, Truncated: truncated})
}

func (t docsTool) create(ctx context.Context, raw json.RawMessage) (string, string, error) {
	payload, err := t.resolveCreatePayload(ctx, raw)
	if err != nil {
		return "", "", err
	}
	if t.approvals == nil {
		return "", "", fmt.Errorf("feishu document creation approval workflow is unavailable")
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal approved document request: %w", err)
	}
	approval, err := t.approvals.CheckOrRequest(ctx, OperationApprovalRequest{
		ToolName:      createToolName,
		ActionKey:     "create",
		ResourceType:  "folder",
		ResourceToken: payload.FolderToken,
		Fields: []ApprovalField{
			{Label: "文档标题", Value: payload.Title},
			{Label: "目标文件夹", Value: payload.FolderToken},
			{Label: "初始内容", Value: fmt.Sprintf("%d 个字符", utf8.RuneCountInString(payload.Content))},
		},
		Payload: payloadJSON,
	})
	if err != nil {
		return "", "", fmt.Errorf("check or request feishu document creation approval: %w", err)
	}
	if approval.Status == OperationApprovalStatusGranted {
		request, requestErr := t.store.CreateWorkflowRequest(store.WorkflowRequest{
			AccountID: t.accountID,
			Kind:      store.WorkflowRequestKindFeishuDocsCreate,
			State:     store.WorkflowRequestStateExecuting,
			CreatedAt: t.currentTime(),
		})
		if requestErr != nil {
			return "", "", fmt.Errorf("create feishu document workflow request: %w", requestErr)
		}
		out, createErr := t.createDocument(ctx, request.ID, payload)
		if createErr != nil {
			if out.DocumentID == "" {
				t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
				return "", "", createErr
			}
			t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStatePartial)
			out.Warning = fmt.Sprintf("文档已创建，但后续处理失败：%v。请勿重复创建，可稍后继续处理。", createErr)
			content, marshalErr := marshalToolOutput(out)
			return content, "", marshalErr
		}
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateSucceeded)
		content, marshalErr := marshalToolOutput(out)
		return content, "", marshalErr
	}
	if approval.Status != OperationApprovalStatusPending {
		return "", "", fmt.Errorf("unsupported feishu document creation approval status %q", approval.Status)
	}
	content, marshalErr := marshalToolOutput(pendingApprovalOutput{
		Status:    "pending_approval",
		RequestID: approval.RequestID,
		ExpiresAt: approval.ExpiresAt.UTC().Format(time.RFC3339),
		Message:   "已向本次请求的飞书用户发送授权卡片；可同意本次，或永久允许相同用户、机器人账号、对话和 feishu_docs_create 在当前文件夹中创建文档。批准后会异步创建文档，请勿重复调用。",
	})
	return content, approval.RequestID, marshalErr
}

// OperationApprovalPolicy identifies the exact document operation handled by
// the shared approval service.
func (t docsTool) OperationApprovalPolicy() OperationApprovalPolicy {
	switch t.name {
	case createToolName:
		return OperationApprovalPolicy{
			ToolName:    createToolName,
			ActionKey:   "create",
			Action:      "创建飞书文档",
			SupportsAll: true,
		}
	case appendToolName:
		return OperationApprovalPolicy{
			ToolName:    appendToolName,
			ActionKey:   "append",
			Action:      "追加飞书文档内容",
			SupportsAll: true,
		}
	default:
		return OperationApprovalPolicy{}
	}
}

// ExecuteApproved executes the exact document payload persisted before authorization.
func (t docsTool) ExecuteApproved(ctx context.Context, requestID string, payload json.RawMessage) (OperationApprovalExecution, error) {
	switch t.name {
	case createToolName:
		return t.executeApprovedCreate(ctx, requestID, payload)
	case appendToolName:
		return t.executeApprovedAppend(ctx, requestID, payload)
	default:
		return OperationApprovalExecution{}, fmt.Errorf("tool %q does not support approved execution", t.name)
	}
}

func (t docsTool) executeApprovedCreate(ctx context.Context, requestID string, payload json.RawMessage) (OperationApprovalExecution, error) {
	args, err := t.parseApprovedCreatePayload(payload)
	if err != nil {
		return OperationApprovalExecution{}, err
	}
	executionCtx := WithActor(ctx, Actor{OpenID: args.ActorOpenID, UserID: args.ActorUserID})
	executionCtx = WithChatContext(executionCtx, ChatContext{ChatID: args.ChatID})
	if err := t.validateDocumentCreateAccess(executionCtx, args); err != nil {
		return OperationApprovalExecution{}, fmt.Errorf("revalidate approved document target access: %w", err)
	}
	out, err := t.createDocument(executionCtx, requestID, args)
	if err != nil {
		if out.DocumentID != "" {
			return OperationApprovalExecution{
				Message:       fmt.Sprintf("⚠️ 文档已创建，但后续处理失败：[%s](%s)。请勿重复创建，可稍后继续处理。", escapeFeishuLinkText(out.Title), out.URL),
				Warning:       true,
				WarningReason: err.Error(),
			}, nil
		}
		return OperationApprovalExecution{}, err
	}
	return OperationApprovalExecution{
		Message: fmt.Sprintf("✅ 飞书文档已创建：[%s](%s)", escapeFeishuLinkText(out.Title), out.URL),
	}, nil
}

func (t docsTool) executeApprovedAppend(ctx context.Context, requestID string, payload json.RawMessage) (OperationApprovalExecution, error) {
	args, err := t.parseApprovedAppendPayload(payload)
	if err != nil {
		return OperationApprovalExecution{}, err
	}
	executionCtx := WithActor(ctx, Actor{OpenID: args.ActorOpenID, UserID: args.ActorUserID})
	executionCtx = WithChatContext(executionCtx, ChatContext{ChatID: args.ChatID})
	out, err := t.appendDocument(executionCtx, requestID, args)
	if err != nil {
		return OperationApprovalExecution{}, err
	}
	return OperationApprovalExecution{
		Message: fmt.Sprintf("✅ 已追加飞书文档内容：[%s](%s)", escapeFeishuLinkText(out.DocumentID), out.URL),
	}, nil
}

func (t docsTool) parseCreateArgs(raw json.RawMessage) (createArgs, error) {
	var args createArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return createArgs{}, fmt.Errorf("parse arguments: %w", err)
	}
	args.Title = strings.TrimSpace(args.Title)
	args.FolderToken = strings.TrimSpace(args.FolderToken)
	if args.Title == "" {
		return createArgs{}, fmt.Errorf("title is required")
	}
	if utf8.RuneCountInString(args.Title) > maxDocxTitle {
		return createArgs{}, fmt.Errorf("title must not exceed %d characters", maxDocxTitle)
	}
	return args, nil
}

func (t docsTool) resolveCreatePayload(ctx context.Context, raw json.RawMessage) (approvedCreatePayload, error) {
	args, err := t.parseCreateArgs(raw)
	if err != nil {
		return approvedCreatePayload{}, err
	}
	actor, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return approvedCreatePayload{}, err
	}
	folder, err := t.resolveChatFolder(chat.ChatID, args.FolderToken)
	if err != nil {
		return approvedCreatePayload{}, err
	}
	args.FolderToken = folder.FolderToken
	payload := approvedCreatePayload{
		createArgs:  args,
		ChatID:      chat.ChatID,
		ActorOpenID: actor.OpenID,
		ActorUserID: actor.UserID,
	}
	if err := t.validateDocumentCreateAccess(ctx, payload); err != nil {
		return approvedCreatePayload{}, err
	}
	return payload, nil
}

func (t docsTool) parseApprovedCreatePayload(raw json.RawMessage) (approvedCreatePayload, error) {
	var payload approvedCreatePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return approvedCreatePayload{}, fmt.Errorf("parse approved document request: %w", err)
	}
	argsJSON, err := json.Marshal(payload.createArgs)
	if err != nil {
		return approvedCreatePayload{}, fmt.Errorf("normalize approved document request: %w", err)
	}
	args, err := t.parseCreateArgs(argsJSON)
	if err != nil {
		return approvedCreatePayload{}, err
	}
	payload.createArgs = args
	payload.ChatID = strings.TrimSpace(payload.ChatID)
	payload.ActorOpenID = strings.TrimSpace(payload.ActorOpenID)
	payload.ActorUserID = strings.TrimSpace(payload.ActorUserID)
	if payload.ChatID == "" {
		return approvedCreatePayload{}, fmt.Errorf("approved document request is missing chat_id")
	}
	folder, err := t.resolveChatFolder(payload.ChatID, payload.FolderToken)
	if err != nil {
		return approvedCreatePayload{}, err
	}
	payload.FolderToken = folder.FolderToken
	return payload, nil
}

func (t docsTool) resolveChatFolder(chatID, folderToken string) (store.FeishuChatFolder, error) {
	chatID = strings.TrimSpace(chatID)
	folderToken = strings.TrimSpace(folderToken)
	var (
		folder store.FeishuChatFolder
		err    error
	)
	if folderToken == "" {
		folder, err = t.store.DefaultFeishuChatFolder(t.accountID, chatID)
	} else {
		folder, err = t.store.GetFeishuChatFolder(t.accountID, chatID, folderToken)
	}
	if err != nil {
		if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
			return store.FeishuChatFolder{}, fmt.Errorf("folder_token is not a Bot-owned folder bound to the current Feishu chat; call %s first", folderCreateToolName)
		}
		return store.FeishuChatFolder{}, fmt.Errorf("resolve current chat folder: %w", err)
	}
	if folder.ShareState != store.FeishuFolderShareStateSucceeded {
		return store.FeishuChatFolder{}, fmt.Errorf("folder sharing is incomplete; retry its create_request_id before using it")
	}
	return folder, nil
}

func (t docsTool) validateDocumentCreateAccess(ctx context.Context, payload approvedCreatePayload) error {
	if err := requireResourceAccess(ctx, t.resourceAccess, ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: payload.FolderToken,
		Permission:    ResourcePermissionWrite,
	}); err != nil {
		return fmt.Errorf("require target folder access: %w", err)
	}
	feishuToolsLog.Debug(ctx, "validated feishu document create access account=%s chat=%s folder_ref=%s",
		t.accountID, payload.ChatID, hashString(payload.FolderToken))
	return nil
}

func (t docsTool) createDocument(ctx context.Context, requestID string, payload approvedCreatePayload) (writeOutput, error) {
	args := payload.createArgs
	if err := requireResourceAccess(ctx, t.resourceAccess, ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: args.FolderToken,
		Permission:    ResourcePermissionWrite,
	}); err != nil {
		return writeOutput{}, fmt.Errorf("revalidate target folder access: %w", err)
	}
	req := larkdocx.NewCreateDocumentReqBuilder().
		Body(larkdocx.NewCreateDocumentReqBodyBuilder().
			Title(args.Title).
			FolderToken(args.FolderToken).
			Build()).
		Build()
	resp, err := t.client.Docx.Document.Create(ctx, req)
	if err != nil {
		return writeOutput{}, fmt.Errorf("create feishu document: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return writeOutput{}, fmt.Errorf("create feishu document: empty response")
		}
		return writeOutput{}, fmt.Errorf("create feishu document code=%d msg=%s", resp.Code, resp.Msg)
	}
	docID := ""
	if resp.Data != nil && resp.Data.Document != nil {
		docID = deref(resp.Data.Document.DocumentId)
	}
	if docID == "" {
		return writeOutput{}, fmt.Errorf("create feishu document returned no document_id")
	}
	out := writeOutput{RequestID: requestID, DocumentID: docID, Title: args.Title, URL: "https://docs.feishu.cn/docx/" + docID}
	if _, err := t.store.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       t.accountID,
		ResourceType:    "docx",
		ResourceToken:   docID,
		ParentToken:     args.FolderToken,
		Name:            args.Title,
		URL:             out.URL,
		SourceRequestID: requestID,
		CreatedAt:       t.currentTime(),
	}); err != nil {
		return out, fmt.Errorf("record created Feishu document ownership: %w", err)
	}
	if _, err := t.store.SaveFeishuChatDocument(store.FeishuChatDocument{
		AccountID:       t.accountID,
		ChatID:          payload.ChatID,
		DocumentToken:   docID,
		FolderToken:     args.FolderToken,
		Title:           args.Title,
		URL:             out.URL,
		SourceRequestID: requestID,
		CreatedAt:       t.currentTime(),
	}); err != nil {
		return out, fmt.Errorf("record created Feishu document: %w", err)
	}
	if strings.TrimSpace(args.Content) != "" {
		if err := t.appendTextBlocks(ctx, docID, args.Content); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (t docsTool) authorizeDocumentAccess(ctx context.Context, chat ChatContext, documentToken, permission string) (store.FeishuChatDocument, bool, error) {
	documentToken = strings.TrimSpace(documentToken)
	permission = strings.ToLower(strings.TrimSpace(permission))
	if err := requireResourceAccess(ctx, t.resourceAccess, ResourceAccessRequirement{
		ResourceType:  "docx",
		ResourceToken: documentToken,
		Permission:    permission,
	}); err != nil {
		return store.FeishuChatDocument{}, false, fmt.Errorf("require document access: %w", err)
	}
	document, err := t.store.GetFeishuChatDocument(t.accountID, chat.ChatID, documentToken)
	if err == nil {
		return document, true, nil
	}
	if !errors.Is(err, store.ErrFeishuChatDocumentNotFound) {
		return store.FeishuChatDocument{}, false, fmt.Errorf("check current chat document: %w", err)
	}

	resource, err := t.store.GetFeishuBotResource(t.accountID, "docx", documentToken)
	if err == nil {
		parentToken := strings.TrimSpace(resource.ParentToken)
		folder, folderErr := t.store.GetFeishuChatFolder(t.accountID, chat.ChatID, parentToken)
		if folderErr != nil || folder.ShareState != store.FeishuFolderShareStateSucceeded {
			if folderErr != nil && !errors.Is(folderErr, store.ErrFeishuChatFolderNotFound) {
				return store.FeishuChatDocument{}, false, fmt.Errorf("check Bot-owned document parent folder: %w", folderErr)
			}
			return store.FeishuChatDocument{}, false, fmt.Errorf("Bot-owned document is not available to the current Feishu chat")
		}
		createdAt := resource.CreatedAt
		if createdAt.IsZero() {
			createdAt = t.currentTime()
		}
		documentURL := strings.TrimSpace(resource.URL)
		if documentURL == "" {
			documentURL = "https://docs.feishu.cn/docx/" + documentToken
		}
		document, saveErr := t.store.SaveFeishuChatDocument(store.FeishuChatDocument{
			AccountID:       t.accountID,
			ChatID:          chat.ChatID,
			DocumentToken:   documentToken,
			FolderToken:     parentToken,
			Title:           resource.Name,
			URL:             documentURL,
			SourceRequestID: resource.SourceRequestID,
			CreatedAt:       createdAt,
		})
		if saveErr != nil {
			return store.FeishuChatDocument{}, false, fmt.Errorf("repair current chat document binding: %w", saveErr)
		}
		feishuToolsLog.Info(ctx, "repaired Bot-owned feishu document binding account=%s chat=%s document_ref=%s folder_ref=%s",
			t.accountID, chat.ChatID, hashString(documentToken), hashString(parentToken))
		return document, true, nil
	}
	if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		return store.FeishuChatDocument{}, false, fmt.Errorf("check Bot-owned document metadata: %w", err)
	}
	feishuToolsLog.Debug(ctx, "validated external feishu document access account=%s chat=%s document_ref=%s permission=%s",
		t.accountID, chat.ChatID, hashString(documentToken), permission)
	return store.FeishuChatDocument{}, false, nil
}

func (t docsTool) append(ctx context.Context, raw json.RawMessage) (string, string, error) {
	payload, err := t.resolveAppendPayload(ctx, raw)
	if err != nil {
		return "", "", err
	}
	if t.approvals == nil {
		return "", "", fmt.Errorf("feishu document append approval workflow is unavailable")
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal approved document append request: %w", err)
	}
	approval, err := t.approvals.CheckOrRequest(ctx, OperationApprovalRequest{
		ToolName:      appendToolName,
		ActionKey:     "append",
		ResourceType:  "docx",
		ResourceToken: payload.DocumentToken,
		Fields: []ApprovalField{
			{Label: "目标文档", Value: payload.DocumentToken},
			{Label: "追加内容", Value: fmt.Sprintf("%d 个字符", utf8.RuneCountInString(payload.Content))},
		},
		Payload: payloadJSON,
	})
	if err != nil {
		return "", "", fmt.Errorf("check or request feishu document append approval: %w", err)
	}
	if approval.Status == OperationApprovalStatusPending {
		content, marshalErr := marshalToolOutput(pendingApprovalOutput{
			Status:    "pending_approval",
			RequestID: approval.RequestID,
			ExpiresAt: approval.ExpiresAt.UTC().Format(time.RFC3339),
			Message:   "已向本次请求的飞书用户发送授权卡片；可同意本次，或永久允许相同用户、机器人账号、对话和 feishu_docs_append 对当前文档执行追加。批准后会异步追加内容，请勿重复调用。",
		})
		return content, approval.RequestID, marshalErr
	}
	if approval.Status != OperationApprovalStatusGranted {
		return "", "", fmt.Errorf("unsupported feishu document append approval status %q", approval.Status)
	}
	request, err := t.store.CreateWorkflowRequest(store.WorkflowRequest{
		AccountID: t.accountID,
		Kind:      store.WorkflowRequestKindFeishuDocsAppend,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: t.currentTime(),
	})
	if err != nil {
		return "", "", fmt.Errorf("create feishu document append workflow request: %w", err)
	}
	out, err := t.appendDocument(ctx, request.ID, payload)
	if err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", "", err
	}
	t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateSucceeded)
	content, marshalErr := marshalToolOutput(out)
	return content, "", marshalErr
}

func (t docsTool) resolveAppendPayload(ctx context.Context, raw json.RawMessage) (approvedAppendPayload, error) {
	var args appendArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return approvedAppendPayload{}, fmt.Errorf("parse arguments: %w", err)
	}
	if strings.TrimSpace(args.Content) == "" {
		return approvedAppendPayload{}, fmt.Errorf("content is required")
	}
	ref, err := parseDocRef(args.Token, args.URL, "docx")
	if err != nil {
		return approvedAppendPayload{}, err
	}
	if ref.Kind != "docx" {
		return approvedAppendPayload{}, fmt.Errorf("append supports docx documents only")
	}
	actor, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return approvedAppendPayload{}, err
	}
	payload := approvedAppendPayload{
		DocumentToken: ref.Token,
		Content:       args.Content,
		FolderToken:   strings.TrimSpace(args.FolderToken),
		ChatID:        chat.ChatID,
		ActorOpenID:   actor.OpenID,
		ActorUserID:   actor.UserID,
	}
	if err := t.validateAppendAccess(ctx, payload); err != nil {
		return approvedAppendPayload{}, err
	}
	return payload, nil
}

func (t docsTool) parseApprovedAppendPayload(raw json.RawMessage) (approvedAppendPayload, error) {
	var payload approvedAppendPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return approvedAppendPayload{}, fmt.Errorf("parse approved document append request: %w", err)
	}
	payload.DocumentToken = strings.TrimSpace(payload.DocumentToken)
	payload.FolderToken = strings.TrimSpace(payload.FolderToken)
	payload.ChatID = strings.TrimSpace(payload.ChatID)
	payload.ActorOpenID = strings.TrimSpace(payload.ActorOpenID)
	payload.ActorUserID = strings.TrimSpace(payload.ActorUserID)
	if strings.TrimSpace(payload.Content) == "" {
		return approvedAppendPayload{}, fmt.Errorf("approved document append content is required")
	}
	ref, err := parseDocRef(payload.DocumentToken, "", "docx")
	if err != nil {
		return approvedAppendPayload{}, err
	}
	payload.DocumentToken = ref.Token
	if payload.ChatID == "" || (payload.ActorOpenID == "" && payload.ActorUserID == "") {
		return approvedAppendPayload{}, fmt.Errorf("approved document append request is missing trusted chat or actor identity")
	}
	return payload, nil
}

func (t docsTool) validateAppendAccess(ctx context.Context, payload approvedAppendPayload) error {
	document, bound, err := t.authorizeDocumentAccess(ctx, ChatContext{ChatID: payload.ChatID}, payload.DocumentToken, ResourcePermissionWrite)
	if err != nil {
		return err
	}
	if folderToken := payload.FolderToken; folderToken != "" {
		if !bound {
			return fmt.Errorf("folder_token can only validate a document bound to the current Feishu chat")
		}
		if folderToken != document.FolderToken {
			return fmt.Errorf("folder_token does not match the document binding in the current Feishu chat")
		}
	}
	return nil
}

func (t docsTool) appendDocument(ctx context.Context, requestID string, payload approvedAppendPayload) (writeOutput, error) {
	if err := t.validateAppendAccess(ctx, payload); err != nil {
		return writeOutput{}, fmt.Errorf("revalidate approved document append access: %w", err)
	}
	if err := t.appendTextBlocks(ctx, payload.DocumentToken, payload.Content); err != nil {
		return writeOutput{}, err
	}
	return writeOutput{
		RequestID:  requestID,
		DocumentID: payload.DocumentToken,
		URL:        "https://docs.feishu.cn/docx/" + payload.DocumentToken,
		Appended:   true,
	}, nil
}

func (t docsTool) appendTextBlocks(ctx context.Context, documentID, content string) error {
	blocks := textBlocks(content)
	if len(blocks) == 0 {
		return nil
	}
	index := 0
	childrenReq := larkdocx.NewGetDocumentBlockChildrenReqBuilder().
		DocumentId(documentID).
		BlockId(documentID).
		DocumentRevisionId(-1).
		PageSize(500).
		Build()
	children, err := t.client.Docx.DocumentBlockChildren.Get(ctx, childrenReq)
	if err == nil && children != nil && children.Success() && children.Data != nil {
		index = len(children.Data.Items)
	}
	req := larkdocx.NewCreateDocumentBlockChildrenReqBuilder().
		DocumentId(documentID).
		BlockId(documentID).
		DocumentRevisionId(-1).
		Body(larkdocx.NewCreateDocumentBlockChildrenReqBodyBuilder().
			Children(blocks).
			Index(index).
			Build()).
		Build()
	resp, err := t.client.Docx.DocumentBlockChildren.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("append feishu document: %w", err)
	}
	if resp == nil || !resp.Success() {
		return fmt.Errorf("append feishu document code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func textBlocks(content string) []*larkdocx.Block {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	blocks := make([]*larkdocx.Block, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		text := larkdocx.NewTextBuilder().
			Elements([]*larkdocx.TextElement{
				larkdocx.NewTextElementBuilder().
					TextRun(larkdocx.NewTextRunBuilder().Content(line).Build()).
					Build(),
			}).
			Build()
		blocks = append(blocks, larkdocx.NewBlockBuilder().BlockType(docxTextBlock).Text(text).Build())
	}
	return blocks
}

type docRef struct {
	Kind  string
	Token string
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

func parseDocRef(token, rawURL, kind string) (docRef, error) {
	kind = normalizeDocKind(kind)
	token = strings.TrimSpace(token)
	rawURL = strings.TrimSpace(rawURL)
	if token == "" && rawURL == "" {
		return docRef{}, fmt.Errorf("token or url is required")
	}
	if token == "" {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return docRef{}, fmt.Errorf("parse url: %w", err)
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			switch part {
			case "docx", "docs", "doc":
				if i+1 < len(parts) {
					kind = "docx"
					token = strings.TrimSpace(parts[i+1])
				}
			case "wiki":
				if i+1 < len(parts) {
					kind = "wiki"
					token = strings.TrimSpace(parts[i+1])
				}
			case "file":
				if i+1 < len(parts) {
					kind = "file"
					token = strings.TrimSpace(parts[i+1])
				}
			}
		}
	}
	if token == "" {
		return docRef{}, fmt.Errorf("document token not found")
	}
	if !tokenPattern.MatchString(token) {
		return docRef{}, fmt.Errorf("invalid document token")
	}
	if kind == "" {
		kind = "docx"
	}
	return docRef{Kind: kind, Token: token}, nil
}

func normalizeDocKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "doc", "docs", "document":
		return "docx"
	default:
		return kind
	}
}

func docsSearchSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        searchToolName,
		Description: "Search docx documents only inside Bot-owned folders bound to the current trusted Feishu chat. Returns titles, summaries, URLs, and tokens.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search keywords."},"max_items":{"type":"integer","minimum":1,"maximum":20}},"required":["query"],"additionalProperties":false}`),
	}
}

func docsReadSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        readToolName,
		Description: "Read plain text from a Feishu docx document. The tool checks Bot ownership or the current trusted user's scoped docx/read grant and live Feishu capability without sending cards itself. If authorization is missing it returns resource_authorization_required; call feishu_docs_request_access with the returned resource and permission, then retry. External access does not create a permanent chat binding. The result may be truncated by configuration.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","description":"Feishu docx document token."},"url":{"type":"string","description":"Feishu document URL."},"type":{"type":"string","enum":["docx","wiki","file"],"description":"Document type hint."}},"additionalProperties":false}`),
	}
}

func docsCreateSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        createToolName,
		Description: "Create a Feishu docx document only in a Bot-owned folder bound to the current trusted chat. The tool checks folder/write access from trusted context before approval and again immediately before creation; missing authorization returns resource_authorization_required without sending a resource card. Omit folder_token to use the chat's default Bot folder. To place a document in a non-Bot-owned directory, use the create-in-Bot-folder, copy-to-target, then delete-temporary-resource flow. A matching operation grant executes immediately; otherwise creation starts after the requester approves the operation card.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","minLength":1,"maxLength":800},"content":{"type":"string"},"folder_token":{"type":"string","description":"Optional Bot-owned folder token already bound to this exact Feishu chat."}},"required":["title"],"additionalProperties":false}`),
	}
}

func escapeFeishuLinkText(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.NewReplacer(
		"\\", "\\\\",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
	).Replace(value)
}

func docsAppendSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        appendToolName,
		Description: "Append plain text paragraphs to a Feishu docx document. The tool first checks Bot ownership or the current trusted user's scoped docx/write grant and live Feishu capability without sending a resource card. If resource authorization is missing it returns resource_authorization_required; call feishu_docs_request_access with the returned resource and permission, then retry. After resource access succeeds, a matching append-operation grant executes immediately; otherwise the requester must approve an operation card. The append grant is isolated to this exact document and does not inherit from create or other tools. External access does not create a permanent chat binding.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"token":{"type":"string"},"url":{"type":"string"},"content":{"type":"string"},"folder_token":{"type":"string","description":"Optional consistency check available only for a current-chat document binding."}},"required":["content"],"additionalProperties":false}`),
	}
}

func availableFeishuFolders(folders []store.FeishuChatFolder) []store.FeishuChatFolder {
	available := make([]store.FeishuChatFolder, 0, len(folders))
	for _, folder := range folders {
		if folder.ShareState == store.FeishuFolderShareStateSucceeded {
			available = append(available, folder)
		}
	}
	return available
}

func folderRequiredForFolders(folders []store.FeishuChatFolder) folderRequiredOutput {
	out := folderRequiredOutput{
		Status:       "folder_required",
		RequiredTool: folderCreateToolName,
		Message:      "当前飞书对话没有可用的 Bot 自有目录，请先创建目录。",
	}
	for _, folder := range folders {
		if folder.ShareState != store.FeishuFolderShareStateSucceeded && folder.CreateRequestID != "" {
			out.Message = "当前飞书对话的目录创建尚未完整结束，请先重试该目录请求。"
			out.CreateRequest = folder.CreateRequestID
			break
		}
	}
	return out
}

func folderDisplayName(folder store.FeishuChatFolder) string {
	if name := strings.TrimSpace(folder.Name); name != "" {
		return name
	}
	return "未命名目录"
}

func isWikiSearchResult(result searchResult) bool {
	return strings.EqualFold(strings.TrimSpace(result.Type), "wiki")
}

func searchResultKey(result searchResult) string {
	if token := strings.TrimSpace(result.Token); token != "" {
		return "token:" + token
	}
	if resultURL := strings.TrimSpace(result.URL); resultURL != "" {
		return "url:" + resultURL
	}
	return "title:" + strings.TrimSpace(result.Title) + "\x00" + strings.TrimSpace(result.Summary)
}

func (t docsTool) updateWorkflowBestEffort(ctx context.Context, requestID, state string) {
	if err := t.store.UpdateWorkflowRequestState(requestID, t.accountID, state, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "update feishu document workflow failed request=%s account=%s state=%s: %v", shortToolRequestID(requestID), t.accountID, state, err)
	}
}

func (t docsTool) currentTime() time.Time {
	if t.now == nil {
		return time.Now().UTC()
	}
	return t.now().UTC()
}

func stripSearchHighlight(text string) string {
	text = strings.ReplaceAll(text, "<h>", "")
	text = strings.ReplaceAll(text, "</h>", "")
	return html.UnescapeString(text)
}

func marshalToolOutput(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func truncateRunes(text string, limit int) (string, bool) {
	if limit <= 0 {
		return text, false
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	return string(runes[:limit]), true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
