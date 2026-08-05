package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

func (m *resourceAccessManager) resolveResource(ctx context.Context, chat feishutools.ChatContext, input feishutools.ResourceAccessRequest) (string, string, string, error) {
	resourceType := feishutools.NormalizeResourceType(input.ResourceType)
	resourceToken := strings.TrimSpace(input.ResourceToken)
	resourceURL := strings.TrimSpace(input.ResourceURL)
	switch resourceToken {
	case feishutools.BotRootResourceAlias:
		root, err := m.applicationRootFolder(ctx)
		if err != nil {
			return "", "", "", err
		}
		resourceToken = root.Token
		if resourceURL == "" {
			resourceURL = defaultFeishuResourceURL("folder", resourceToken)
		}
		if _, err := m.store.SaveFeishuBotResource(store.FeishuBotResource{
			AccountID:     m.account.ID,
			ResourceType:  "folder",
			ResourceToken: resourceToken,
			Name:          "Bot Root",
			URL:           resourceURL,
			CreatedAt:     m.currentTime(),
		}); err != nil {
			return "", "", "", fmt.Errorf("record feishu Bot root ownership: %w", err)
		}
	case feishutools.ChatDefaultFolderResourceAlias:
		folder, err := m.store.DefaultFeishuChatFolder(m.account.ID, chat.ChatID)
		if err != nil {
			if errors.Is(err, store.ErrFeishuChatFolderNotFound) {
				return "", "", "", fmt.Errorf("current Feishu chat has no default Bot folder; create one first")
			}
			return "", "", "", fmt.Errorf("resolve current chat default folder: %w", err)
		}
		resourceToken = folder.FolderToken
		if resourceURL == "" {
			resourceURL = folder.URL
		}
		if _, err := m.store.SaveFeishuBotResource(store.FeishuBotResource{
			AccountID:       m.account.ID,
			ResourceType:    "folder",
			ResourceToken:   folder.FolderToken,
			ParentToken:     folder.ParentFolderToken,
			Name:            folder.Name,
			URL:             folder.URL,
			SourceRequestID: folder.CreateRequestID,
			CreatedAt:       folder.CreatedAt,
		}); err != nil {
			return "", "", "", fmt.Errorf("record current chat Bot folder ownership: %w", err)
		}
	}
	if resourceURL == "" {
		resourceURL = defaultFeishuResourceURL(resourceType, resourceToken)
	}
	return resourceType, resourceToken, resourceURL, nil
}

func (m *resourceAccessManager) resolveResourceDisplayName(chat feishutools.ChatContext, resourceType, resourceToken string) (string, error) {
	resourceType = feishutools.NormalizeResourceType(resourceType)
	resourceToken = strings.TrimSpace(resourceToken)
	resource, err := m.store.GetFeishuBotResource(m.account.ID, resourceType, resourceToken)
	if err == nil {
		if name := normalizeResourceDisplayName(resource.Name); name != "" {
			return name, nil
		}
	} else if !errors.Is(err, store.ErrFeishuBotResourceNotFound) {
		return "", fmt.Errorf("resolve Bot-owned feishu resource display name: %w", err)
	}

	switch resourceType {
	case "folder":
		folder, folderErr := m.store.GetFeishuChatFolder(m.account.ID, chat.ChatID, resourceToken)
		if folderErr == nil {
			if name := normalizeResourceDisplayName(folder.Name); name != "" {
				return name, nil
			}
		} else if !errors.Is(folderErr, store.ErrFeishuChatFolderNotFound) {
			return "", fmt.Errorf("resolve chat-bound feishu folder display name: %w", folderErr)
		}
	case "docx":
		document, documentErr := m.store.GetFeishuChatDocument(m.account.ID, chat.ChatID, resourceToken)
		if documentErr == nil {
			if name := normalizeResourceDisplayName(document.Title); name != "" {
				return name, nil
			}
		} else if !errors.Is(documentErr, store.ErrFeishuChatDocumentNotFound) {
			return "", fmt.Errorf("resolve chat-bound feishu document display name: %w", documentErr)
		}
	}
	return fallbackResourceDisplayName(resourceType, resourceToken), nil
}

func (m *resourceAccessManager) resourceAccessOAuthDisplayStatus(actor feishutools.Actor, capabilityActive bool) (string, error) {
	if capabilityActive {
		return resourceAccessOAuthStatusCapabilityReady, nil
	}
	if !m.oauthEnabled() {
		return resourceAccessOAuthStatusConfigurationMissing, nil
	}
	credential, err := m.store.GetFeishuUserOAuthCredential(m.account.ID, actor.OpenID, actor.UserID)
	if err != nil {
		if errors.Is(err, store.ErrFeishuUserOAuthCredentialNotFound) {
			return resourceAccessOAuthStatusAuthorizationNeeded, nil
		}
		return "", err
	}
	now := m.currentTime()
	metadataUsable := credential.Status == store.FeishuUserOAuthCredentialStatusActive &&
		credential.ReauthorizeAt.After(now) &&
		len(missingOAuthScopes(credential.Scopes, resourceAccessOAuthScope)) == 0 &&
		((credential.AccessTokenCiphertext != "" && credential.AccessTokenExpiresAt.After(now)) ||
			(credential.RefreshTokenCiphertext != "" && credential.RefreshTokenExpiresAt.After(now)))
	if metadataUsable {
		return resourceAccessOAuthStatusCredentialReady, nil
	}
	return resourceAccessOAuthStatusAuthorizationNeeded, nil
}

func normalizeResourceDisplayName(value string) string {
	return truncateApprovalRunes(strings.TrimSpace(value), resourceAccessDisplayNameRunes)
}

func fallbackResourceDisplayName(resourceType, resourceToken string) string {
	label := map[string]string{
		"folder":  "飞书文件夹",
		"docx":    "飞书文档",
		"doc":     "飞书文档",
		"sheet":   "飞书电子表格",
		"bitable": "飞书多维表格",
		"wiki":    "飞书知识库页面",
		"file":    "飞书文件",
	}[feishutools.NormalizeResourceType(resourceType)]
	if label == "" {
		label = "飞书资源"
	}
	if reference := shortResourceRef(resourceToken); reference != "" {
		return label + "（" + reference + "）"
	}
	return label
}

type resourceAccessRootFolder struct {
	Token string
}

func (m *resourceAccessManager) applicationRootFolder(ctx context.Context) (resourceAccessRootFolder, error) {
	resp, err := m.client.Get(ctx, "/open-apis/drive/explorer/v2/root_folder/meta", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder: %w", err)
	}
	if resp == nil {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder: empty response")
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return resourceAccessRootFolder{}, fmt.Errorf("parse feishu application root folder: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Code != 0 {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	result.Data.Token = strings.TrimSpace(result.Data.Token)
	if result.Data.Token == "" {
		return resourceAccessRootFolder{}, fmt.Errorf("get feishu application root folder returned no token")
	}
	return resourceAccessRootFolder{Token: result.Data.Token}, nil
}
