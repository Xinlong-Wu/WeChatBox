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
	resourceAccessOAuthResultField      = "information_1"
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
		"点击下方“前往飞书官方授权页面”按钮完成授权。",
		"LingoBridge 仅使用本次返回的 user_access_token 完成授权，不保存 user_access_token 或 refresh_token。",
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
		"config": map[string]interface{}{
			"update_multi": true,
			"style": map[string]interface{}{
				"text_size": map[string]interface{}{
					"normal_v2": map[string]interface{}{
						"default": "normal",
						"pc":      "normal",
						"mobile":  "heading",
					},
				},
			},
		},
		"header": map[string]interface{}{
			"title": map[string]interface{}{"tag": "plain_text", "content": "飞书文档权限申请"},
			"subtitle": map[string]interface{}{
				"tag":     "plain_text",
				"content": "",
			},
			"text_tag_list": []interface{}{
				map[string]interface{}{
					"tag":   "text_tag",
					"text":  map[string]interface{}{"tag": "plain_text", "content": "安全加密"},
					"color": "blue",
				},
			},
			"template": "blue",
			"padding":  "12px 8px 12px 8px",
		},
		"body": map[string]interface{}{
			"direction": "vertical",
			"elements": []interface{}{
				map[string]interface{}{
					"tag": "div",
					"text": map[string]interface{}{
						"tag":        "plain_text",
						"content":    "飞书资源授权",
						"text_size":  "normal_v2",
						"text_align": "left",
						"text_color": "default",
					},
					"margin": "0px 0px 0px 0px",
				},
				map[string]interface{}{
					"tag":        "markdown",
					"content":    "为了更好地为您提供服务，需要收集以下必要信息。\n" + strings.Join(lines, "\n"),
					"text_align": "left",
					"text_size":  "normal_v2",
					"margin":     "0px 0px 0px 0px",
					"element_id": "KNJPSduXTksKaRe28qq6",
				},
				map[string]interface{}{
					"tag":   "button",
					"text":  map[string]interface{}{"tag": "plain_text", "content": "前往飞书官方授权页面"},
					"type":  "primary",
					"width": "default",
					"size":  "medium",
					"behaviors": []interface{}{
						map[string]interface{}{
							"type":        "open_url",
							"default_url": c.authURL,
							"android_url": "",
							"ios_url":     "",
							"pc_url":      "",
						},
					},
					"margin": "0px 0px 0px 0px",
				},
				map[string]interface{}{
					"tag": "form",
					"elements": []interface{}{
						map[string]interface{}{
							"tag": "input",
							"placeholder": map[string]interface{}{
								"tag":     "plain_text",
								"content": "",
							},
							"default_value": "",
							"width":         "default",
							"label": map[string]interface{}{
								"tag":     "plain_text",
								"content": "授权回调 URL 或授权码",
							},
							"label_position": "left",
							"required":       false,
							"name":           resourceAccessOAuthResultField,
							"element_id":     "e45nAhDEUoVmMTaWcZKP",
						},
						map[string]interface{}{
							"tag":   "button",
							"text":  map[string]interface{}{"tag": "plain_text", "content": "提交授权结果"},
							"type":  "primary_filled",
							"width": "fill",
							"behaviors": []interface{}{
								map[string]interface{}{"type": "callback", "value": value(resourceAccessCardActionSubmitOAuth)},
							},
							"form_action_type": "submit",
							"name":             "submit_btn",
							"margin":           "4px 0px 4px 0px",
							"element_id":       "yJZDKLb72aTt6mKHuVam",
						},
						map[string]interface{}{
							"tag":                "column_set",
							"horizontal_spacing": "8px",
							"horizontal_align":   "left",
							"columns": []interface{}{
								map[string]interface{}{
									"tag":    "column",
									"width":  "weighted",
									"weight": 1,
									"elements": []interface{}{
										map[string]interface{}{
											"tag": "input",
											"placeholder": map[string]interface{}{
												"tag":     "plain_text",
												"content": "请输入建议",
											},
											"default_value": "",
											"width":         "fill",
											"name":          "Input_9luq5y9ljxa",
											"margin":        "0px 0px 0px 0px",
										},
									},
									"vertical_align": "top",
								},
								map[string]interface{}{
									"tag":   "column",
									"width": "auto",
									"elements": []interface{}{
										map[string]interface{}{
											"tag":   "button",
											"text":  map[string]interface{}{"tag": "plain_text", "content": "拒绝"},
											"type":  "danger",
											"width": "default",
											"size":  "medium",
											"behaviors": []interface{}{
												map[string]interface{}{"type": "callback", "value": value(resourceAccessCardActionReject)},
											},
											"form_action_type": "submit",
											"name":             "Button_ylh56j56ycl",
											"margin":           "0px 0px 0px 0px",
										},
									},
									"vertical_align": "top",
								},
							},
							"margin": "0px 0px 0px 0px",
						},
					},
					"direction":        "vertical",
					"horizontal_align": "left",
					"vertical_align":   "top",
					"padding":          "12px 12px 12px 12px",
					"margin":           "0px 0px 0px 0px",
					"name":             "privacy_form",
					"element_id":       "STIJ_lgxwvFvn9xFUnT8",
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
