package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
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
	name    string
	spec    tooltypes.Spec
	service *docsFolderService
}

// docsFolderService owns Feishu folder authorization, persistence, sharing,
// remote API orchestration, and create reconciliation. docsFolderTool remains
// the thin provider-facing adapter for one tool name and schema.
type docsFolderService struct {
	client                *lark.Client
	store                 *store.Store
	accountID             string
	resourceAccess        ResourceAccessGuard
	remoteReconcileDelays []time.Duration
	now                   func() time.Time
}

// NewDocsFolderTools returns chat-scoped application-folder tools.
func NewDocsFolderTools(client *lark.Client, st *store.Store, accountID string, cfg Config, resourceAccess ResourceAccessGuard) []tooltypes.Tool {
	cfg = NormalizeConfig(cfg)
	accountID = strings.TrimSpace(accountID)
	if client == nil || st == nil || st.PlatformID() != store.PlatformFeishu || accountID == "" || !cfg.Docs.Enabled {
		return nil
	}
	service := &docsFolderService{
		client:                client,
		store:                 st,
		accountID:             accountID,
		resourceAccess:        resourceAccess,
		remoteReconcileDelays: copyRemoteCreateReconciliationDelays(),
		now:                   time.Now,
	}
	tools := []tooltypes.Tool{
		docsFolderTool{name: folderListToolName, spec: docsFolderListSpec(), service: service},
	}
	if cfg.Docs.AllowWrite && resourceAccess != nil {
		tools = append([]tooltypes.Tool{
			docsFolderTool{name: folderCreateToolName, spec: docsFolderCreateSpec(), service: service},
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
	if t.service == nil {
		err = fmt.Errorf("feishu docs folder service is unavailable")
		return tooltypes.Result{CallID: call.ID, Name: t.name, Content: contentOrError("", err), IsError: true}
	}
	switch t.name {
	case folderCreateToolName:
		content, err = t.service.createFolder(ctx, call.Arguments)
	case folderListToolName:
		content, err = t.service.listFolders(ctx, call.Arguments)
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

var _ tooltypes.Tool = docsFolderTool{}

type folderCreateArgs struct {
	Name              string `json:"name,omitempty"`
	ParentFolderToken string `json:"parent_folder_token,omitempty"`
	SetDefault        bool   `json:"set_default,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
}

type folderCreateOutput struct {
	Status            string `json:"status"`
	RequestID         string `json:"request_id"`
	FolderToken       string `json:"folder_token,omitempty"`
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

func (t *docsFolderService) createFolder(ctx context.Context, raw json.RawMessage) (string, error) {
	var args folderCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	args.ParentFolderToken = strings.TrimSpace(args.ParentFolderToken)
	args.RequestID = strings.TrimSpace(args.RequestID)
	actor, chat, err := trustedDocsScope(ctx)
	if err != nil {
		return "", err
	}
	if args.RequestID != "" {
		if args.Name != "" || args.ParentFolderToken != "" || args.SetDefault {
			return "", fmt.Errorf("request_id retry must not include name, parent_folder_token, or set_default")
		}
		return t.recoverFolderCreate(ctx, actor, chat, args.RequestID)
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
	parentToken := args.ParentFolderToken
	createParentToken := parentToken
	expectedOwnerID := ""
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
		expectedOwnerID = root.UserID
		feishuToolsLog.Debug(ctx, "resolved feishu application root account=%s root_id=%s owner_id=%s", t.accountID, root.ID, root.UserID)
	}
	if _, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: createParentToken,
		Permission:    ResourcePermissionWrite,
	}); err != nil {
		return "", fmt.Errorf("require parent folder access: %w", err)
	}
	feishuToolsLog.Debug(ctx, "validated feishu folder create access account=%s chat=%s parent_ref=%s",
		t.accountID, chat.ChatID, hashString(createParentToken))
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
	payloadHash, err := remoteOperationPayloadHash(struct {
		OperationKind      string `json:"operation_kind"`
		ChatID             string `json:"chat_id"`
		ActorOpenID        string `json:"actor_open_id,omitempty"`
		ActorUserID        string `json:"actor_user_id,omitempty"`
		ParentToken        string `json:"parent_token"`
		BindingParentToken string `json:"binding_parent_token,omitempty"`
		Name               string `json:"name"`
		SetDefault         bool   `json:"set_default"`
		ShareMemberType    string `json:"share_member_type"`
		ShareMemberID      string `json:"share_member_id"`
	}{
		OperationKind:      store.FeishuRemoteOperationKindFolderCreate,
		ChatID:             chat.ChatID,
		ActorOpenID:        actor.OpenID,
		ActorUserID:        actor.UserID,
		ParentToken:        createParentToken,
		BindingParentToken: parentToken,
		Name:               args.Name,
		SetDefault:         args.SetDefault,
		ShareMemberType:    shareMemberType,
		ShareMemberID:      shareMemberID,
	})
	if err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", err
	}
	operation, err := t.store.PrepareFeishuRemoteOperation(store.FeishuRemoteOperation{
		RequestID:           request.ID,
		AccountID:           t.accountID,
		OperationKind:       store.FeishuRemoteOperationKindFolderCreate,
		ChatID:              chat.ChatID,
		ActorOpenID:         actor.OpenID,
		ActorUserID:         actor.UserID,
		ParentResourceType:  "folder",
		ParentResourceToken: createParentToken,
		BindingParentToken:  parentToken,
		RequestedName:       args.Name,
		PayloadHash:         payloadHash,
		SetDefault:          args.SetDefault,
		ShareMemberType:     shareMemberType,
		ShareMemberID:       shareMemberID,
		RemoteResourceType:  "folder",
		CreatedAt:           now,
	})
	if err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", fmt.Errorf("prepare feishu folder remote operation: %w", err)
	}
	parent, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: createParentToken,
		Permission:    ResourcePermissionWrite,
	})
	if err != nil {
		t.updateWorkflowBestEffort(ctx, request.ID, store.WorkflowRequestStateFailed)
		return "", fmt.Errorf("revalidate parent folder access: %w", err)
	}
	feishuToolsLog.Info(ctx, "creating feishu application folder request=%s account=%s chat=%s parent_ref=%s name_chars=%d",
		shortToolRequestID(request.ID), t.accountID, chat.ChatID, hashString(createParentToken), utf8.RuneCountInString(args.Name))
	return t.continueFolderCreate(ctx, operation, parent, expectedOwnerID)
}

func (t *docsFolderService) retryFolderShare(ctx context.Context, actor Actor, chat ChatContext, requestID string) (string, error) {
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
	authorized, err := requireResourceAccess(ctx, t.resourceAccess, t.accountID, ResourceAccessRequirement{
		ResourceType:  "folder",
		ResourceToken: folder.FolderToken,
		Permission:    ResourcePermissionWrite,
	})
	if err != nil {
		return "", fmt.Errorf("authorize feishu folder sharing: %w", err)
	}
	feishuToolsLog.Info(ctx, "retrying feishu folder share request=%s account=%s chat=%s folder_ref=%s target_type=%s",
		shortToolRequestID(requestID), t.accountID, chat.ChatID, hashString(folder.FolderToken), folder.ShareMemberType)
	if err := t.ensureFolderFullAccess(ctx, folder, authorized); err != nil {
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

func (t *docsFolderService) listFolders(ctx context.Context, raw json.RawMessage) (string, error) {
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

func (t *docsFolderService) applicationRootFolder(ctx context.Context) (applicationRootFolder, error) {
	return getApplicationRootFolder(ctx, t.client)
}

func (t *docsFolderService) ensureFolderFullAccess(ctx context.Context, folder store.FeishuChatFolder, authorized AuthorizedResource) error {
	if authorized.AccountID != folder.AccountID || authorized.ChatID != folder.ChatID ||
		!authorizedResourcePermits(authorized, "folder", folder.FolderToken, ResourcePermissionWrite) {
		return fmt.Errorf("grant feishu folder full access: authorized folder/write resource does not match the stored chat folder")
	}
	memberType := larkdrive.TypeCreatePermissionMemberUser
	if folder.ShareMemberType == larkdrive.MemberTypeOpenChat {
		memberType = larkdrive.TypeCreatePermissionMemberChat
	}
	createReq := larkdrive.NewCreatePermissionMemberReqBuilder().
		Token(authorized.ResourceToken).
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
		if createResp.Code == 1063003 {
			return t.upgradeFolderCollaboratorToFullAccess(ctx, folder, authorized)
		}
		return fmt.Errorf("grant feishu folder full access code=%d msg=%s", createResp.Code, createResp.Msg)
	}
	return nil
}

func (t *docsFolderService) upgradeFolderCollaboratorToFullAccess(ctx context.Context, folder store.FeishuChatFolder, authorized AuthorizedResource) error {
	memberType := larkdrive.TypeUpdatePermissionMemberUser
	if folder.ShareMemberType == larkdrive.MemberTypeOpenChat {
		memberType = larkdrive.TypeUpdatePermissionMemberChat
	}
	updateReq := larkdrive.NewUpdatePermissionMemberReqBuilder().
		Token(authorized.ResourceToken).
		MemberId(folder.ShareMemberID).
		Type(larkdrive.TokenTypeV2CreatePermissionMemberFolder).
		BaseMember(larkdrive.NewBaseMemberBuilder().
			MemberType(folder.ShareMemberType).
			MemberId(folder.ShareMemberID).
			Perm(larkdrive.PermUpdatePermissionMemberFullAccess).
			Type(memberType).
			Build()).
		Build()
	updateResp, err := t.client.Drive.PermissionMember.Update(ctx, updateReq)
	if err != nil {
		return fmt.Errorf("upgrade existing feishu folder collaborator to full access: %w", err)
	}
	if updateResp == nil {
		return fmt.Errorf("upgrade existing feishu folder collaborator to full access: empty response")
	}
	if !updateResp.Success() {
		return fmt.Errorf("upgrade existing feishu folder collaborator to full access code=%d msg=%s", updateResp.Code, updateResp.Msg)
	}
	feishuToolsLog.Info(ctx, "upgraded existing feishu folder collaborator to full access request=%s account=%s chat=%s folder_ref=%s target_type=%s",
		shortToolRequestID(folder.CreateRequestID), folder.AccountID, folder.ChatID, hashString(folder.FolderToken), folder.ShareMemberType)
	return nil
}

func (t *docsFolderService) updateFolderShareBestEffort(ctx context.Context, folder store.FeishuChatFolder, state string) {
	if err := t.store.UpdateFeishuChatFolderShareState(folder.AccountID, folder.ChatID, folder.FolderToken, state, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "update feishu folder share state failed request=%s account=%s chat=%s folder_ref=%s state=%s: %v",
			shortToolRequestID(folder.CreateRequestID), folder.AccountID, folder.ChatID, hashString(folder.FolderToken), state, err)
	}
}

func (t *docsFolderService) updateWorkflowBestEffort(ctx context.Context, requestID, state string) {
	if err := t.store.UpdateWorkflowRequestState(requestID, t.accountID, state, t.currentTime()); err != nil {
		feishuToolsLog.Warn(ctx, "update feishu folder workflow failed request=%s account=%s state=%s: %v", shortToolRequestID(requestID), t.accountID, state, err)
	}
}

func (t *docsFolderService) currentTime() time.Time {
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
		Description: "Create a Bot-owned Feishu folder under the Bot root or another Bot-owned folder bound to the current trusted chat, then grant the chat or private-chat user full access. The tool checks folder/write access from trusted context before creation and returns resource_authorization_required if it is missing; it does not send a resource card itself. This operation has no separate operation-approval card. If creation is uncertain or later persistence/sharing is incomplete, retry with only request_id; recovery never repeats the create API call.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Folder name for a new folder; maximum 256 UTF-8 bytes."},"parent_folder_token":{"type":"string","description":"Optional Bot-owned parent folder already bound to this exact Feishu chat. Omit to use the Bot root."},"set_default":{"type":"boolean","description":"Make the new folder the default for this chat. The first folder is always default."},"request_id":{"type":"string","description":"Recover a prior partial or uncertain creation without repeating the create API call; when set, omit all other fields."}},"oneOf":[{"required":["name"]},{"required":["request_id"]}],"additionalProperties":false}`),
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
