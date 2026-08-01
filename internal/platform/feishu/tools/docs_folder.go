package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const (
	folderCreateToolName = "feishu_docs_folder_create"
	folderListToolName   = "feishu_docs_folder_list"
	maxFolderNameBytes   = 256
)

type docsFolderTool struct {
	name           string
	spec           tooltypes.Spec
	client         *lark.Client
	store          *store.Store
	accountID      string
	resourceAccess ResourceAccessController
	now            func() time.Time
}

// NewDocsFolderTools returns chat-scoped application-folder tools.
func NewDocsFolderTools(client *lark.Client, st *store.Store, accountID string, cfg Config, resourceAccess ResourceAccessController) []tooltypes.Tool {
	cfg = NormalizeConfig(cfg)
	accountID = strings.TrimSpace(accountID)
	if client == nil || st == nil || st.PlatformID() != store.PlatformFeishu || accountID == "" || !cfg.Docs.Enabled {
		return nil
	}
	tools := []tooltypes.Tool{
		docsFolderTool{name: folderListToolName, spec: docsFolderListSpec(), client: client, store: st, accountID: accountID, now: time.Now},
	}
	if cfg.Docs.AllowWrite && resourceAccess != nil {
		tools = append([]tooltypes.Tool{
			docsFolderTool{name: folderCreateToolName, spec: docsFolderCreateSpec(), client: client, store: st, accountID: accountID, resourceAccess: resourceAccess, now: time.Now},
		}, tools...)
	}
	return tools
}

func (t docsFolderTool) Spec() tooltypes.Spec {
	return t.spec
}

func (t docsFolderTool) Execute(ctx context.Context, call tooltypes.Call) tooltypes.Result {
	var content string
	var err error
	switch t.name {
	case folderCreateToolName:
		content, err = t.createFolder(ctx, call.Arguments)
	case folderListToolName:
		content, err = t.listFolders(ctx, call.Arguments)
	default:
		err = fmt.Errorf("unsupported feishu docs folder tool %q", t.name)
	}
	return tooltypes.Result{
		CallID:  call.ID,
		Name:    t.name,
		Content: contentOrError(content, err),
		IsError: err != nil,
	}
}

type folderCreateArgs struct {
	Name              string `json:"name,omitempty"`
	ParentFolderToken string `json:"parent_folder_token,omitempty"`
	SetDefault        bool   `json:"set_default,omitempty"`
	AccessRequestID   string `json:"access_request_id,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
}

type folderCreateOutput struct {
	Status            string `json:"status"`
	RequestID         string `json:"request_id"`
	FolderToken       string `json:"folder_token"`
	Name              string `json:"name"`
	URL               string `json:"url,omitempty"`
	ParentFolderToken string `json:"parent_folder_token,omitempty"`
	Default           bool   `json:"default"`
	Shared            bool   `json:"shared"`
	Warning           string `json:"warning,omitempty"`
	Retry             string `json:"retry,omitempty"`
}

type folderListOutput struct {
	Folders []folderListItem `json:"folders"`
}

type folderListItem struct {
	FolderToken       string `json:"folder_token"`
	Name              string `json:"name"`
	URL               string `json:"url,omitempty"`
	ParentFolderToken string `json:"parent_folder_token,omitempty"`
	Default           bool   `json:"default"`
	Shared            bool   `json:"shared"`
	CreateRequestID   string `json:"create_request_id"`
}

type applicationRootFolder struct {
	Token  string
	ID     string
	UserID string
}

func (t docsFolderTool) createFolder(ctx context.Context, raw json.RawMessage) (string, error) {
	var args folderCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	args.ParentFolderToken = strings.TrimSpace(args.ParentFolderToken)
	args.AccessRequestID = strings.TrimSpace(args.AccessRequestID)
	args.RequestID = strings.TrimSpace(args.RequestID)
	actor, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return "", err
	}
	if args.RequestID != "" {
		if args.Name != "" || args.ParentFolderToken != "" || args.SetDefault || args.AccessRequestID != "" {
			return "", fmt.Errorf("request_id retry must not include name, parent_folder_token, set_default, or access_request_id")
		}
		return t.retryFolderShare(ctx, actor, chat, args.RequestID)
	}
	if args.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len([]byte(args.Name)) > maxFolderNameBytes {
		return "", fmt.Errorf("name must not exceed %d bytes", maxFolderNameBytes)
	}
	if utf8.RuneCountInString(args.Name) == 0 {
		return "", fmt.Errorf("name is required")
	}
	if args.AccessRequestID == "" {
		return "", fmt.Errorf("access_request_id is required; call %s with folder/write before creating a folder", ResourceAccessToolName)
	}
	parentToken := args.ParentFolderToken
	createParentToken := parentToken
	if parentToken != "" {
		parent, err := t.store.GetFeishuChatFolder(t.accountID, chat.ChatID, parentToken)
		if err != nil {
			if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
				return "", fmt.Errorf("parent_folder_token is not bound to the current Feishu chat")
			}
			return "", fmt.Errorf("check parent feishu folder: %w", err)
		}
		if parent.ShareState != store.FeishuFolderShareStateSucceeded {
			return "", fmt.Errorf("parent folder sharing is incomplete; retry its create_request_id before creating a child folder")
		}
	} else {
		root, err := t.applicationRootFolder(ctx)
		if err != nil {
			return "", err
		}
		createParentToken = root.Token
		feishuToolsLog.Debug(ctx, "resolved feishu application root account=%s root_id=%s owner_id=%s", t.accountID, root.ID, root.UserID)
	}
	if _, err := validateGrantedResourceAccess(ctx, t.resourceAccess, ResourceAccessValidation{
		RequestID:     args.AccessRequestID,
		ResourceType:  "folder",
		ResourceToken: createParentToken,
		Permission:    ResourcePermissionWrite,
	}); err != nil {
		return "", fmt.Errorf("validate parent folder access: %w", err)
	}
	feishuToolsLog.Debug(ctx, "validated feishu folder create access request=%s account=%s chat=%s parent_ref=%s",
		shortToolRequestID(args.AccessRequestID), t.accountID, chat.ChatID, hashString(createParentToken))
	shareMemberType, shareMemberID, err := folderShareTarget(actor, chat)
	if err != nil {
		return "", err
	}
	now := t.currentTime()
	request, err := t.store.CreateWorkflowRequest(store.WorkflowRequest{
		AccountID: t.accountID,
		Kind:      store.WorkflowRequestKindFeishuFolderCreate,
		State:     store.WorkflowRequestStateExecuting,
		CreatedAt: now,
	})
	if err != nil {
		return "", fmt.Errorf("create feishu folder workflow request: %w", err)
	}
	if _, err := consumeGrantedResourceAccess(ctx, t.resourceAccess, ResourceAccessValidation{
		RequestID:     args.AccessRequestID,
		ResourceType:  "folder",
		ResourceToken: createParentToken,
		Permission:    ResourcePermissionWrite,
	}, request.ID); err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", fmt.Errorf("consume parent folder access: %w", err)
	}
	feishuToolsLog.Info(ctx, "creating feishu application folder request=%s account=%s chat=%s parent_ref=%s name_chars=%d",
		shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(createParentToken), utf8.RuneCountInString(args.Name))
	created, err := t.createApplicationFolder(ctx, args.Name, createParentToken)
	if err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", fmt.Errorf("create feishu application folder: %w", err)
	}
	if _, err := t.store.SaveFeishuBotResource(store.FeishuBotResource{
		AccountID:       t.accountID,
		ResourceType:    "folder",
		ResourceToken:   created.FolderToken,
		ParentToken:     createParentToken,
		Name:            args.Name,
		URL:             created.URL,
		SourceRequestID: request.ID,
		CreatedAt:       now,
	}); err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStatePartial)
		feishuToolsLog.Error(ctx, "persist created feishu folder ownership failed request=%s account=%s chat=%s folder_ref=%s: %v",
			shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(created.FolderToken), err)
		return marshalToolOutput(folderCreateOutput{
			Status:            "partial",
			RequestID:         request.ID,
			FolderToken:       created.FolderToken,
			Name:              args.Name,
			URL:               created.URL,
			ParentFolderToken: parentToken,
			Shared:            false,
			Warning:           "文件夹已创建，但 Bot 资源归属记录失败。请勿用相同名称重复创建；请联系管理员检查数据库。",
		})
	}
	folder, saveErr := t.store.SaveFeishuChatFolder(store.FeishuChatFolder{
		AccountID:         t.accountID,
		ChatID:            chat.ChatID,
		FolderToken:       created.FolderToken,
		Name:              args.Name,
		URL:               created.URL,
		ParentFolderToken: parentToken,
		Default:           args.SetDefault,
		ShareMemberType:   shareMemberType,
		ShareMemberID:     shareMemberID,
		ShareState:        store.FeishuFolderShareStatePending,
		CreateRequestID:   request.ID,
		CreatedByOpenID:   actor.OpenID,
		CreatedByUserID:   actor.UserID,
		CreatedAt:         now,
	})
	if saveErr != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStatePartial)
		feishuToolsLog.Error(ctx, "persist created feishu folder failed request=%s account=%s chat=%s folder_ref=%s: %v",
			shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(created.FolderToken), saveErr)
		return marshalToolOutput(folderCreateOutput{
			Status:            "partial",
			RequestID:         request.ID,
			FolderToken:       created.FolderToken,
			Name:              args.Name,
			URL:               created.URL,
			ParentFolderToken: parentToken,
			Shared:            false,
			Warning:           "文件夹已创建，但本地对话绑定保存失败。请勿用相同名称重复创建；请联系管理员检查数据库。",
		})
	}
	if err := t.ensureFolderFullAccess(ctx, folder, false); err != nil {
		t.updateFolderShareBestEffort(ctx, folder, store.FeishuFolderShareStateFailed)
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStatePartial)
		feishuToolsLog.Warn(ctx, "share feishu folder failed request=%s account=%s chat=%s folder_ref=%s target_type=%s: %v",
			shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(folder.FolderToken), folder.ShareMemberType, err)
		return marshalToolOutput(folderCreateOutput{
			Status:            "partial",
			RequestID:         request.ID,
			FolderToken:       folder.FolderToken,
			Name:              folder.Name,
			URL:               folder.URL,
			ParentFolderToken: folder.ParentFolderToken,
			Default:           folder.Default,
			Shared:            false,
			Warning:           "文件夹已创建，但未能授予当前对话完全访问权限。请勿重新创建文件夹。",
			Retry:             fmt.Sprintf("再次调用 %s，并且只传 request_id=%s，以仅重试授权。", folderCreateToolName, request.ID),
		})
	}
	if err := t.store.UpdateFeishuChatFolderShareState(folder.AccountID, folder.ChatID, folder.FolderToken, store.FeishuFolderShareStateSucceeded, t.currentTime()); err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStatePartial)
		feishuToolsLog.Warn(ctx, "record feishu folder share success failed request=%s account=%s chat=%s folder_ref=%s: %v",
			shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(folder.FolderToken), err)
		return marshalToolOutput(folderCreateOutput{
			Status:            "partial",
			RequestID:         request.ID,
			FolderToken:       folder.FolderToken,
			Name:              folder.Name,
			URL:               folder.URL,
			ParentFolderToken: folder.ParentFolderToken,
			Default:           folder.Default,
			Shared:            true,
			Warning:           "文件夹已创建并授权，但本地状态尚未确认。",
			Retry:             fmt.Sprintf("再次调用 %s，并且只传 request_id=%s，以核验并完成记录。", folderCreateToolName, request.ID),
		})
	}
	if err := t.store.UpdateWorkflowRequestState(request.ID, t.accountID, store.WorkflowRequestStateSucceeded, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "mark feishu folder workflow succeeded failed request=%s account=%s: %v", shortToolRequestID(request.ID), t.accountID, err)
		folder.ShareState = store.FeishuFolderShareStateSucceeded
		return marshalToolOutput(folderOutput(
			folder,
			"partial",
			true,
			"文件夹已创建并授权，但请求状态尚未完成记录。",
			fmt.Sprintf("再次调用 %s，并且只传 request_id=%s，以完成状态记录。", folderCreateToolName, request.ID),
		))
	}
	folder.ShareState = store.FeishuFolderShareStateSucceeded
	feishuToolsLog.Info(ctx, "created and shared feishu application folder request=%s account=%s chat=%s folder_ref=%s target_type=%s default=%t",
		shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(folder.FolderToken), folder.ShareMemberType, folder.Default)
	return marshalToolOutput(folderOutput(folder, "created", true, "", ""))
}

func (t docsFolderTool) retryFolderShare(ctx context.Context, actor Actor, chat ChatContext, requestID string) (string, error) {
	if actor.OpenID == "" && actor.UserID == "" {
		return "", fmt.Errorf("feishu folder retry requires the requesting user identity")
	}
	folder, err := t.store.GetFeishuChatFolderByRequest(t.accountID, chat.ChatID, requestID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
			return "", fmt.Errorf("request_id does not identify a folder in the current Feishu chat")
		}
		return "", fmt.Errorf("load feishu folder retry: %w", err)
	}
	if folder.ShareState == store.FeishuFolderShareStateSucceeded {
		if err := t.store.UpdateWorkflowRequestState(requestID, t.accountID, store.WorkflowRequestStateSucceeded, t.currentTime()); err != nil {
			feishuToolsLog.Warn(ctx, "repair succeeded feishu folder workflow failed request=%s account=%s: %v", shortToolRequestID(requestID), t.accountID, err)
		}
		return marshalToolOutput(folderOutput(folder, "created", true, "", ""))
	}
	feishuToolsLog.Info(ctx, "retrying feishu folder share request=%s account=%s chat=%s folder_ref=%s target_type=%s",
		shortToolRequestID(requestID), t.accountID, chat.ChatID, hashString(folder.FolderToken), folder.ShareMemberType)
	if err := t.ensureFolderFullAccess(ctx, folder, folder.ShareState == store.FeishuFolderShareStatePending); err != nil {
		t.updateFolderShareBestEffort(ctx, folder, store.FeishuFolderShareStateFailed)
		t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
		return marshalToolOutput(folderOutput(
			folder,
			"partial",
			false,
			"文件夹仍未能授予当前对话完全访问权限。",
			fmt.Sprintf("稍后再次调用 %s，并且只传 request_id=%s。", folderCreateToolName, requestID),
		))
	}
	if err := t.store.UpdateFeishuChatFolderShareState(folder.AccountID, folder.ChatID, folder.FolderToken, store.FeishuFolderShareStateSucceeded, t.currentTime()); err != nil {
		t.updateWorkflowBestEffort(ctx, requestID, store.WorkflowRequestStatePartial)
		return "", fmt.Errorf("record feishu folder share retry: %w", err)
	}
	if err := t.store.UpdateWorkflowRequestState(requestID, t.accountID, store.WorkflowRequestStateSucceeded, t.currentTime()); err != nil {
		return "", fmt.Errorf("complete feishu folder workflow retry: %w", err)
	}
	folder.ShareState = store.FeishuFolderShareStateSucceeded
	feishuToolsLog.Info(ctx, "completed feishu folder share retry request=%s account=%s chat=%s folder_ref=%s",
		shortToolRequestID(requestID), t.accountID, chat.ChatID, hashString(folder.FolderToken))
	return marshalToolOutput(folderOutput(folder, "created", true, "", ""))
}

func (t docsFolderTool) listFolders(ctx context.Context, raw json.RawMessage) (string, error) {
	if len(strings.TrimSpace(string(raw))) > 0 && strings.TrimSpace(string(raw)) != "{}" {
		var args map[string]interface{}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("parse arguments: %w", err)
		}
		if len(args) > 0 {
			return "", fmt.Errorf("feishu_docs_folder_list does not accept arguments")
		}
	}
	_, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return "", err
	}
	folders, err := t.store.ListFeishuChatFolders(t.accountID, chat.ChatID)
	if err != nil {
		return "", fmt.Errorf("list current feishu chat folders: %w", err)
	}
	out := folderListOutput{Folders: make([]folderListItem, 0, len(folders))}
	for _, folder := range folders {
		out.Folders = append(out.Folders, folderListItem{
			FolderToken:       folder.FolderToken,
			Name:              folder.Name,
			URL:               folder.URL,
			ParentFolderToken: folder.ParentFolderToken,
			Default:           folder.Default,
			Shared:            folder.ShareState == store.FeishuFolderShareStateSucceeded,
			CreateRequestID:   folder.CreateRequestID,
		})
	}
	feishuToolsLog.Debug(ctx, "listed feishu chat folders account=%s chat=%s count=%d", t.accountID, chat.ChatID, len(out.Folders))
	return marshalToolOutput(out)
}

func trustedDocsScope(ctx context.Context) (Actor, ChatContext, error) {
	actor, ok := ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return Actor{}, ChatContext{}, fmt.Errorf("feishu docs tools require the requesting user identity")
	}
	chat, ok := ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return Actor{}, ChatContext{}, fmt.Errorf("feishu docs tools require the trusted current chat")
	}
	return actor, chat, nil
}

func folderShareTarget(actor Actor, chat ChatContext) (string, string, error) {
	if chat.IsGroup {
		return larkdrive.MemberTypeOpenChat, chat.ChatID, nil
	}
	if actor.OpenID == "" {
		return "", "", fmt.Errorf("private-chat folder sharing requires the requesting user's open_id")
	}
	return larkdrive.MemberTypeOpenId, actor.OpenID, nil
}

func (t docsFolderTool) applicationRootFolder(ctx context.Context) (applicationRootFolder, error) {
	resp, err := t.client.Get(ctx, "/open-apis/drive/explorer/v2/root_folder/meta", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder: %w", err)
	}
	if resp == nil {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder: empty response")
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token  string `json:"token"`
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return applicationRootFolder{}, fmt.Errorf("parse feishu application root folder: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	root := applicationRootFolder{
		Token:  strings.TrimSpace(result.Data.Token),
		ID:     strings.TrimSpace(result.Data.ID),
		UserID: strings.TrimSpace(result.Data.UserID),
	}
	if root.Token == "" {
		return applicationRootFolder{}, fmt.Errorf("get feishu application root folder returned no token")
	}
	return root, nil
}

func (t docsFolderTool) createApplicationFolder(ctx context.Context, name, parentToken string) (store.FeishuChatFolder, error) {
	req := larkdrive.NewCreateFolderFileReqBuilder().
		Body(larkdrive.NewCreateFolderFileReqBodyBuilder().Name(name).FolderToken(parentToken).Build()).
		Build()
	resp, err := t.client.Drive.File.CreateFolder(ctx, req)
	if err != nil {
		return store.FeishuChatFolder{}, err
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return store.FeishuChatFolder{}, fmt.Errorf("empty response")
		}
		return store.FeishuChatFolder{}, fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || strings.TrimSpace(deref(resp.Data.Token)) == "" {
		return store.FeishuChatFolder{}, fmt.Errorf("response missing folder token")
	}
	token := strings.TrimSpace(deref(resp.Data.Token))
	url := strings.TrimSpace(deref(resp.Data.Url))
	if url == "" {
		url = "https://docs.feishu.cn/drive/folder/" + token
	}
	return store.FeishuChatFolder{FolderToken: token, URL: url}, nil
}

func (t docsFolderTool) ensureFolderFullAccess(ctx context.Context, folder store.FeishuChatFolder, acceptExisting bool) error {
	memberType := larkdrive.TypeCreatePermissionMemberUser
	if folder.ShareMemberType == larkdrive.MemberTypeOpenChat {
		memberType = larkdrive.TypeCreatePermissionMemberChat
	}
	createReq := larkdrive.NewCreatePermissionMemberReqBuilder().
		Token(folder.FolderToken).
		Type(larkdrive.TokenTypeV2CreatePermissionMemberFolder).
		NeedNotification(false).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(folder.ShareMemberType).
			MemberId(folder.ShareMemberID).
			Perm(larkdrive.PermCreatePermissionMemberFullAccess).
			Type(memberType).
			Build()).
		Build()
	createResp, err := t.client.Drive.PermissionMember.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("grant feishu folder full access: %w", err)
	}
	if createResp == nil || !createResp.Success() {
		if createResp == nil {
			return fmt.Errorf("grant feishu folder full access: empty response")
		}
		// A pending local state can mean Feishu accepted the previous request but
		// the local success update failed. In that exact retry case, the official
		// "invalid operation" response is treated as already granted.
		if acceptExisting && createResp.Code == 1063003 {
			feishuToolsLog.Debug(ctx, "feishu folder collaborator already exists during retry request=%s folder_ref=%s", shortToolRequestID(folder.CreateRequestID), hashString(folder.FolderToken))
			return nil
		}
		return fmt.Errorf("grant feishu folder full access code=%d msg=%s", createResp.Code, createResp.Msg)
	}
	return nil
}

func (t docsFolderTool) updateFolderShareBestEffort(ctx context.Context, folder store.FeishuChatFolder, state string) {
	if err := t.store.UpdateFeishuChatFolderShareState(folder.AccountID, folder.ChatID, folder.FolderToken, state, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "update feishu folder share state failed request=%s account=%s chat=%s folder_ref=%s state=%s: %v",
			shortToolRequestID(folder.CreateRequestID), folder.AccountID, folder.ChatID, hashString(folder.FolderToken), state, err)
	}
}

func (t docsFolderTool) updateWorkflowBestEffort(ctx context.Context, requestID, state string) {
	if err := t.store.UpdateWorkflowRequestState(requestID, t.accountID, state, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "update feishu folder workflow failed request=%s account=%s state=%s: %v", shortToolRequestID(requestID), t.accountID, state, err)
	}
}

func (t docsFolderTool) currentTime() time.Time {
	if t.now == nil {
		return time.Now().UTC()
	}
	return t.now().UTC()
}

func folderOutput(folder store.FeishuChatFolder, status string, shared bool, warning, retry string) folderCreateOutput {
	return folderCreateOutput{
		Status:            status,
		RequestID:         folder.CreateRequestID,
		FolderToken:       folder.FolderToken,
		Name:              folder.Name,
		URL:               folder.URL,
		ParentFolderToken: folder.ParentFolderToken,
		Default:           folder.Default,
		Shared:            shared,
		Warning:           warning,
		Retry:             retry,
	}
}

func docsFolderCreateSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        folderCreateToolName,
		Description: "Create a Bot-owned Feishu folder under the Bot root or another Bot-owned folder bound to the current trusted chat, then grant the chat or private-chat user full access. Before creating, call feishu_docs_request_access for folder/write on the actual parent and pass its granted request_id as access_request_id. This operation has no separate operation-approval card. If sharing fails after creation, retry with only request_id so the folder is not created twice.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Folder name for a new folder; maximum 256 UTF-8 bytes."},"parent_folder_token":{"type":"string","description":"Optional Bot-owned parent folder already bound to this exact Feishu chat. Omit to use the Bot root."},"set_default":{"type":"boolean","description":"Make the new folder the default for this chat. The first folder is always default."},"access_request_id":{"type":"string","description":"Granted request_id returned by feishu_docs_request_access for write access to the actual parent folder."},"request_id":{"type":"string","description":"Retry a prior partial result by sharing the already-created folder; when set, omit all other fields."}},"oneOf":[{"required":["name","access_request_id"]},{"required":["request_id"]}],"additionalProperties":false}`),
	}
}

func docsFolderListSpec() tooltypes.Spec {
	return tooltypes.Spec{
		Name:        folderListToolName,
		Description: "List only the application-owned Feishu folders bound to the current trusted chat, with the default and sharing status.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func shortToolRequestID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
