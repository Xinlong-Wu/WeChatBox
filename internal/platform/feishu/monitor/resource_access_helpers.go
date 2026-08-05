package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

func (m *resourceAccessManager) currentTime() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *resourceAccessManager) requestTTL() time.Duration {
	if m.ttl <= 0 {
		return defaultResourceAccessTTL
	}
	return m.ttl
}

func (m *resourceAccessManager) resourceCardUpdateTimeout() time.Duration {
	if m.cardUpdateTimeout <= 0 {
		return defaultFeishuCardUpdateTimeout
	}
	return m.cardUpdateTimeout
}

func (m *resourceAccessManager) baseContext() context.Context {
	if m.runCtx == nil {
		return context.Background()
	}
	return m.runCtx
}

func trustedResourceAccessScope(ctx context.Context) (feishutools.Actor, feishutools.ChatContext, error) {
	actor, ok := feishutools.ActorFromContext(ctx)
	if !ok || (actor.OpenID == "" && actor.UserID == "") {
		return feishutools.Actor{}, feishutools.ChatContext{}, fmt.Errorf("feishu resource access requires the requesting user identity")
	}
	chat, ok := feishutools.ChatContextFromContext(ctx)
	if !ok || chat.ChatID == "" {
		return feishutools.Actor{}, feishutools.ChatContext{}, fmt.Errorf("feishu resource access requires the trusted current chat")
	}
	return actor, chat, nil
}

func normalizeResourceAccessRequest(input feishutools.ResourceAccessRequest) (feishutools.ResourceAccessRequest, error) {
	input.ResourceType = feishutools.NormalizeResourceType(input.ResourceType)
	input.ResourceToken = strings.TrimSpace(input.ResourceToken)
	input.ResourceURL = strings.TrimSpace(input.ResourceURL)
	input.Permission = strings.ToLower(strings.TrimSpace(input.Permission))
	input.Reason = strings.TrimSpace(input.Reason)
	if !feishutools.SupportedResourceType(input.ResourceType) || input.ResourceToken == "" {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("valid feishu resource type and token are required")
	}
	if input.Permission != feishutools.ResourcePermissionRead && input.Permission != feishutools.ResourcePermissionWrite {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu resource permission must be read or write")
	}
	if input.OnceDurationMinutes < store.FeishuResourceAccessMinOnceDurationMinutes || input.OnceDurationMinutes > store.FeishuResourceAccessMaxOnceDurationMinutes {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu resource once duration must be between %d and %d minutes", store.FeishuResourceAccessMinOnceDurationMinutes, store.FeishuResourceAccessMaxOnceDurationMinutes)
	}
	if feishutools.ResourceTokenAlias(input.ResourceToken) && input.ResourceType != "folder" {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("feishu folder aliases require resource_type=folder")
	}
	urlToken := input.ResourceToken
	if feishutools.ResourceTokenAlias(urlToken) {
		urlToken = ""
	}
	if input.ResourceURL != "" && !safeFeishuResourceURL(input.ResourceURL, urlToken) {
		return feishutools.ResourceAccessRequest{}, fmt.Errorf("resource_url must be an HTTPS Feishu/Lark resource URL containing resource_token")
	}
	return input, nil
}

func newResourceAccessOAuthValues() (state, stateHash string, err error) {
	state, err = randomBase64URL(32)
	if err != nil {
		return "", "", err
	}
	stateHash = hashResourceAccessState(state)
	return state, stateHash, nil
}

func randomBase64URL(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func hashResourceAccessState(state string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(state)))
	return hex.EncodeToString(sum[:])
}

func shortResourceRef(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func resourceAccessActorID(request store.FeishuResourceAccessRequest) string {
	if request.ActorOpenID != "" {
		return request.ActorOpenID
	}
	return request.ActorUserID
}

func resourceAccessGrantActor(openID, userID string) (string, string, error) {
	if openID = strings.TrimSpace(openID); openID != "" {
		return store.FeishuResourceGrantActorTypeOpenID, openID, nil
	}
	if userID = strings.TrimSpace(userID); userID != "" {
		return store.FeishuResourceGrantActorTypeUserID, userID, nil
	}
	return "", "", fmt.Errorf("trusted feishu resource grant actor is required")
}

func resourceAccessActorMatches(request store.FeishuResourceAccessRequest, actor feishutools.Actor) bool {
	if request.ActorOpenID != "" {
		return request.ActorOpenID == strings.TrimSpace(actor.OpenID)
	}
	return request.ActorUserID != "" && request.ActorUserID == strings.TrimSpace(actor.UserID)
}

func feishuCollaboratorPermissionSatisfies(granted, requested string, folder bool) bool {
	granted = strings.TrimSpace(granted)
	requested = strings.TrimSpace(requested)
	if requested == store.FeishuResourcePermissionRead {
		return granted == larkdrive.PermCreatePermissionMemberView || granted == larkdrive.PermCreatePermissionMemberEdit || granted == larkdrive.PermCreatePermissionMemberFullAccess
	}
	if folder {
		return granted == larkdrive.PermCreatePermissionMemberFullAccess
	}
	return granted == larkdrive.PermCreatePermissionMemberEdit || granted == larkdrive.PermCreatePermissionMemberFullAccess
}

func defaultFeishuResourceURL(resourceType, token string) string {
	token = url.PathEscape(strings.TrimSpace(token))
	switch resourceType {
	case "folder":
		return "https://docs.feishu.cn/drive/folder/" + token
	case "docx":
		return "https://docs.feishu.cn/docx/" + token
	case "doc":
		return "https://docs.feishu.cn/docs/" + token
	case "sheet":
		return "https://docs.feishu.cn/sheets/" + token
	case "bitable":
		return "https://docs.feishu.cn/base/" + token
	case "wiki":
		return "https://docs.feishu.cn/wiki/" + token
	case "file":
		return "https://docs.feishu.cn/file/" + token
	default:
		return ""
	}
}

func redirectToResource(w http.ResponseWriter, r *http.Request, rawURL string) {
	target, err := url.Parse(strings.TrimSpace(rawURL))
	if err == nil && safeFeishuResourceURL(target.String(), "") {
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("授权已完成，可以关闭此页面并返回飞书。"))
}

func safeFeishuResourceURL(rawURL, resourceToken string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.ContainsAny(rawURL, "\r\n\t <>\\()") {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host != "feishu.cn" && !strings.HasSuffix(host, ".feishu.cn") && host != "larksuite.com" && !strings.HasSuffix(host, ".larksuite.com") {
		return false
	}
	resourceToken = strings.TrimSpace(resourceToken)
	return resourceToken == "" || strings.Contains(parsed.EscapedPath(), url.PathEscape(resourceToken)) || strings.Contains(parsed.RawQuery, url.QueryEscape(resourceToken))
}
