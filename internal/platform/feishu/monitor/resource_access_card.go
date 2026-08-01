package monitor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"lingobridge/internal/store"
)

const (
	resourceAccessCardActionKind   = "lingobridge_resource_access"
	resourceAccessCardActionReject = "reject"
)

type pendingResourceAccessCard struct {
	request store.FeishuResourceAccessRequest
	authURL string
}

func (c pendingResourceAccessCard) JSON() (string, error) {
	permissionLabel := "读取"
	if c.request.Permission == store.FeishuResourcePermissionWrite {
		permissionLabel = "写入"
	}
	lines := []string{
		"机器人需要由本次请求的飞书用户授权，才能为当前对话访问该资源。",
		"",
		"**权限**：" + permissionLabel,
		"**资源类型**：" + escapeApprovalMarkdown(c.request.ResourceType),
		"**资源 Token**：`" + escapeApprovalMarkdown(c.request.ResourceToken) + "`",
	}
	if c.request.Reason != "" {
		lines = append(lines, "**用途**："+escapeApprovalMarkdown(c.request.Reason))
	}
	lines = append(lines,
		"",
		"点击下方按钮会打开飞书官方 OAuth 授权页。LingoBridge 仅使用本次返回的 user_access_token 完成授权，不保存 user_access_token 或 refresh_token。",
		fmt.Sprintf("本请求将于 %s 过期。", c.request.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")),
	)
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{"update_multi": true},
		"header": map[string]interface{}{
			"title": map[string]interface{}{"tag": "plain_text", "content": "飞书文档权限申请"},
			"subtitle": map[string]interface{}{
				"tag":     "plain_text",
				"content": permissionLabel + "权限 · " + c.request.ResourceType,
			},
			"text_tag_list": []interface{}{
				map[string]interface{}{
					"tag":   "text_tag",
					"text":  map[string]interface{}{"tag": "plain_text", "content": "待授权"},
					"color": "orange",
				},
			},
			"template": "blue",
		},
		"body": map[string]interface{}{
			"direction":        "vertical",
			"vertical_spacing": "8px",
			"elements": []interface{}{
				map[string]interface{}{
					"tag":     "markdown",
					"content": strings.Join(lines, "\n"),
				},
				map[string]interface{}{
					"tag":  "button",
					"text": map[string]interface{}{"tag": "plain_text", "content": "前往飞书授权"},
					"type": "primary",
					"behaviors": []interface{}{
						map[string]interface{}{
							"type":        "open_url",
							"default_url": c.authURL,
						},
					},
				},
				map[string]interface{}{
					"tag":  "button",
					"text": map[string]interface{}{"tag": "plain_text", "content": "拒绝"},
					"type": "danger",
					"behaviors": []interface{}{
						map[string]interface{}{
							"type": "callback",
							"value": map[string]interface{}{
								"kind":       resourceAccessCardActionKind,
								"request_id": c.request.ID,
								"action":     resourceAccessCardActionReject,
							},
						},
					},
				},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseResourceAccessCardAction(event *callback.CardActionTriggerEvent) (string, string, bool) {
	if cardActionKind(event) != resourceAccessCardActionKind || event == nil || event.Event == nil || event.Event.Action == nil {
		return "", "", false
	}
	requestID := strings.TrimSpace(stringApprovalValue(event.Event.Action.Value, "request_id"))
	action := strings.TrimSpace(stringApprovalValue(event.Event.Action.Value, "action"))
	if requestID == "" || action != resourceAccessCardActionReject {
		return requestID, action, false
	}
	return requestID, action, true
}
