package monitor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"lingobridge/internal/store"
)

const (
	resourceAccessCardActionKind        = "lingobridge_resource_access"
	resourceAccessCardActionSubmitOAuth = "submit_oauth_callback"
	resourceAccessCardActionReject      = "reject"
	resourceAccessOAuthResultField      = "oauth_result"
	resourceAccessOAuthResultMaxLength  = 1000
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
		"如果授权后浏览器无法返回 LingoBridge，请复制地址栏中的完整回调 URL，返回本卡片粘贴并提交；也可以只粘贴授权码。",
		fmt.Sprintf("本请求将于 %s 过期。", c.request.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")),
	)
	value := func(action string) map[string]interface{} {
		return map[string]interface{}{
			"kind":       resourceAccessCardActionKind,
			"request_id": c.request.ID,
			"action":     action,
		}
	}
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
			"direction":          "vertical",
			"horizontal_spacing": "8px",
			"vertical_spacing":   "8px",
			"horizontal_align":   "left",
			"vertical_align":     "top",
			"elements": []interface{}{
				map[string]interface{}{
					"tag":        "markdown",
					"content":    strings.Join(lines, "\n"),
					"text_align": "left",
					"text_size":  "normal",
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
					"tag": "form",
					"elements": []interface{}{
						map[string]interface{}{
							"tag": "input",
							"placeholder": map[string]interface{}{
								"tag":     "plain_text",
								"content": "粘贴完整回调 URL 或授权码",
							},
							"name":       resourceAccessOAuthResultField,
							"required":   true,
							"max_length": resourceAccessOAuthResultMaxLength,
						},
						approvalFormButton("提交授权结果", "primary_filled", "Button_submit_oauth_result", value(resourceAccessCardActionSubmitOAuth)),
					},
					"direction":        "vertical",
					"horizontal_align": "left",
					"vertical_align":   "top",
					"name":             "Form_resource_oauth_result",
				},
				resourceAccessCallbackButton("拒绝", "danger", "Button_reject_resource_access", value(resourceAccessCardActionReject)),
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
	if requestID == "" || (action != resourceAccessCardActionSubmitOAuth && action != resourceAccessCardActionReject) {
		return requestID, action, false
	}
	return requestID, action, true
}

func resourceAccessCardOAuthResult(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return ""
	}
	return strings.TrimSpace(stringApprovalValue(event.Event.Action.FormValue, resourceAccessOAuthResultField))
}

func resourceAccessCallbackButton(text, buttonType, name string, value map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":   "button",
		"text":  map[string]interface{}{"tag": "plain_text", "content": text},
		"type":  buttonType,
		"width": "default",
		"size":  "medium",
		"behaviors": []interface{}{
			map[string]interface{}{"type": "callback", "value": value},
		},
		"name":   name,
		"margin": "4px 0px 4px 0px",
	}
}
