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
	resourceAccessCardActionApproveOnce = "approve_once"
	resourceAccessCardActionApproveAll  = "approve_all"
	resourceAccessCardActionSubmitOAuth = "submit_oauth_callback"
	resourceAccessCardActionReject      = "reject"
	resourceAccessOAuthResultField      = "information_1"
	resourceAccessOAuthResultMaxLength  = 1000
)

type pendingResourceGrantCard struct {
	request     store.FeishuResourceAccessRequest
	oauthStatus string
}

func (c pendingResourceGrantCard) JSON() (string, error) {
	permissionLabel := resourceAccessPermissionLabel(c.request.Permission)
	displayName := resourceAccessCardDisplayName(c.request)
	lines := []string{
		"机器人请求在当前飞书对话中使用以下资源：",
		"",
		"**资源名称**：" + escapeApprovalMarkdown(displayName),
		"**资源类型**：" + escapeApprovalMarkdown(c.request.ResourceType),
		"**权限**：" + permissionLabel,
		"**飞书协作者**：" + escapeApprovalMarkdown(resourceAccessSubjectLabel(c.request)),
		"**OAuth 状态**：" + escapeApprovalMarkdown(resourceAccessOAuthStatusLabel(c.oauthStatus)),
	}
	if c.request.ResourceURL != "" {
		lines = append(lines, "**资源链接**：[在飞书中打开]("+c.request.ResourceURL+")")
	}
	if c.request.Reason != "" {
		lines = append(lines, "**用途**："+escapeApprovalMarkdown(c.request.Reason))
	}
	lines = append(lines,
		"",
		fmt.Sprintf("- **允许 %d 分钟**：飞书能力核验成功后，LingoBridge 在该时限内允许当前用户和对话多次使用此权限。", c.request.OnceDurationMinutes),
		"- **永久允许**：为当前用户、机器人账号、当前对话和这一精确资源保存长期授权。",
		"- 临时授权到期只会让 LingoBridge 停止放行新的操作，不会移除或降低飞书中的 Bot/群聊协作者权限。",
		fmt.Sprintf("本卡片将于 %s 过期。", c.request.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")),
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
			"title":    map[string]interface{}{"tag": "plain_text", "content": "飞书资源权限申请"},
			"subtitle": map[string]interface{}{"tag": "plain_text", "content": permissionLabel},
			"text_tag_list": []interface{}{
				map[string]interface{}{
					"tag":   "text_tag",
					"text":  map[string]interface{}{"tag": "plain_text", "content": "待授权"},
					"color": "orange",
				},
			},
			"template": "blue",
			"padding":  "12px 8px 12px 8px",
		},
		"body": map[string]interface{}{
			"direction":          "vertical",
			"horizontal_spacing": "8px",
			"vertical_spacing":   "8px",
			"horizontal_align":   "left",
			"vertical_align":     "top",
			"elements": []interface{}{
				map[string]interface{}{
					"tag": "form",
					"elements": []interface{}{
						map[string]interface{}{
							"tag":        "markdown",
							"content":    strings.Join(lines, "\n"),
							"text_align": "left",
							"text_size":  "normal",
							"margin":     "0px 0px 0px 0px",
							"element_id": "SnLSJiYBwzi2qzhJsFPP",
						},
						approvalFormButton(fmt.Sprintf("允许 %d 分钟", c.request.OnceDurationMinutes), "primary_filled", "Button_resource_once", value(resourceAccessCardActionApproveOnce)),
						approvalFormButton("永久允许", "primary", "Button_resource_all", value(resourceAccessCardActionApproveAll)),
						approvalReasonRow(value(resourceAccessCardActionReject)),
					},
					"direction":        "vertical",
					"horizontal_align": "left",
					"vertical_align":   "top",
					"padding":          "4px 0px 4px 0px",
					"margin":           "0px 0px 0px 0px",
					"name":             "Form_resource_access",
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

type pendingResourceAccessCard struct {
	request store.FeishuResourceAccessRequest
	authURL string
}

func (c pendingResourceAccessCard) JSON() (string, error) {
	permissionLabel := resourceAccessPermissionLabel(c.request.Permission)
	displayName := resourceAccessCardDisplayName(c.request)
	lines := []string{
		"机器人需要由本次请求的飞书用户授权，才能为当前对话访问该资源。",
		"",
		"**资源名称**：" + escapeApprovalMarkdown(displayName),
		"**资源类型**：" + escapeApprovalMarkdown(c.request.ResourceType),
		"**权限**：" + permissionLabel,
		"**飞书协作者**：" + escapeApprovalMarkdown(resourceAccessSubjectLabel(c.request)),
		"**LingoBridge 授权**：" + escapeApprovalMarkdown(resourceAccessGrantModeLabel(c.request)),
		"**OAuth 状态**：" + escapeApprovalMarkdown(resourceAccessOAuthStatusLabel(resourceAccessOAuthStatusAuthorizationNeeded)),
	}
	if c.request.ResourceURL != "" {
		lines = append(lines, "**资源链接**：[在飞书中打开]("+c.request.ResourceURL+")")
	}
	if c.request.Reason != "" {
		lines = append(lines, "**用途**："+escapeApprovalMarkdown(c.request.Reason))
	}
	if c.request.GrantMode == store.FeishuResourceGrantModeOnce {
		lines = append(lines, fmt.Sprintf("**本地有效期**：授权成功后 %d 分钟；到期只停止 LingoBridge 放行新操作，不撤销飞书协作者权限。", c.request.OnceDurationMinutes))
	} else if c.request.GrantMode == store.FeishuResourceGrantModeAll {
		lines = append(lines, "**本地有效期**：永久；仍只适用于当前用户、机器人账号、对话和这一精确资源。")
	}
	lines = append(lines,
		"",
		"点击下方“前往飞书官方授权页面”按钮完成授权。",
		"LingoBridge 会在本机使用应用密钥加密保存 user_access_token 与 refresh_token，仅用于你批准的飞书资源操作；凭证不会发送给大模型或写入日志。",
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
	if requestID == "" || (action != resourceAccessCardActionApproveOnce && action != resourceAccessCardActionApproveAll && action != resourceAccessCardActionSubmitOAuth && action != resourceAccessCardActionReject) {
		return requestID, action, false
	}
	return requestID, action, true
}

func resourceAccessPermissionLabel(permission string) string {
	if permission == store.FeishuResourcePermissionWrite {
		return "写入（包含读取）"
	}
	return "读取"
}

func resourceAccessSubjectLabel(request store.FeishuResourceAccessRequest) string {
	switch strings.TrimSpace(request.SubjectType) {
	case "openchat":
		return "当前群聊（openchat）"
	case "openid":
		return "机器人（Bot）"
	default:
		return "待核验的机器人或群聊协作者"
	}
}

func resourceAccessGrantModeLabel(request store.FeishuResourceAccessRequest) string {
	switch request.GrantMode {
	case store.FeishuResourceGrantModeOnce:
		return fmt.Sprintf("允许 %d 分钟", request.OnceDurationMinutes)
	case store.FeishuResourceGrantModeAll:
		return "永久允许"
	default:
		return "等待用户选择"
	}
}

func resourceAccessCardDisplayName(request store.FeishuResourceAccessRequest) string {
	if name := normalizeResourceDisplayName(request.ResourceDisplayName); name != "" {
		return name
	}
	return fallbackResourceDisplayName(request.ResourceType, request.ResourceToken)
}

func resourceAccessOAuthStatusLabel(status string) string {
	switch status {
	case resourceAccessOAuthStatusCapabilityReady:
		return "已有可核验的飞书资源权限，批准后无需 OAuth"
	case resourceAccessOAuthStatusCredentialReady:
		return "已保存可能可用的加密 OAuth 凭证，批准后将直接使用并在需要时静默刷新"
	case resourceAccessOAuthStatusConfigurationMissing:
		return "当前机器人账号未配置 OAuth，无法创建新的飞书协作者权限"
	case resourceAccessOAuthStatusAuthorizationNeeded:
		return "批准后需要在飞书官方页面完成 OAuth"
	default:
		return "批准后将核验是否需要 OAuth"
	}
}

func resourceAccessCardOAuthResult(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return ""
	}
	return strings.TrimSpace(stringApprovalValue(event.Event.Action.FormValue, resourceAccessOAuthResultField))
}
