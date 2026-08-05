package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	feishutools "lingobridge/internal/platform/feishu/tools"
	"lingobridge/internal/store"
)

const (
	approvalCardActionKind        = "lingobridge_tool_approval"
	approvalCardActionApproveOnce = "approve_once"
	approvalCardActionApproveAll  = "approve_all"
	approvalCardActionReject      = "reject"
	approvalCardMaxFields         = 8
	approvalCardMaxLabelRunes     = 32
	approvalCardMaxValueRunes     = 256
)

type pendingApprovalCard struct {
	policy   feishutools.OperationApprovalPolicy
	request  feishutools.OperationApprovalRequest
	approval store.ToolApproval
}

func (c pendingApprovalCard) JSON() (string, error) {
	lines := []string{
		"机器人请求执行以下操作：",
		"",
		"**操作**：" + escapeApprovalMarkdown(c.policy.Action),
		"**工具**：" + escapeApprovalMarkdown(c.request.ToolName),
	}
	for _, field := range c.request.Fields {
		lines = append(lines, "**"+escapeApprovalMarkdown(field.Label)+"**："+escapeApprovalMarkdown(field.Value))
	}
	lines = append(lines, "", "- **同意一次**：仅执行当前请求。")
	if c.policy.SupportsAll {
		lines = append(lines, "- **全部同意**：永久允许同一飞书用户、机器人账号、当前对话、工具和动作访问当前资源执行相同操作。")
	}
	lines = append(lines, fmt.Sprintf("本卡片将于 %s 过期。", c.approval.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")))
	value := func(action string) map[string]interface{} {
		return map[string]interface{}{
			"kind":       approvalCardActionKind,
			"request_id": c.approval.ID,
			"action":     action,
		}
	}
	formElements := []interface{}{
		map[string]interface{}{
			"tag":        "markdown",
			"content":    strings.Join(lines, "\n"),
			"text_align": "left",
			"text_size":  "normal",
			"margin":     "0px 0px 0px 0px",
			"element_id": "SnLSJiYBwzi2qzhJsFPP",
		},
		approvalFormButton("同意一次", "primary_filled", "Button_ruivkstdali", value(approvalCardActionApproveOnce)),
	}
	if c.policy.SupportsAll {
		formElements = append(formElements, approvalFormButton("全部同意", "primary", "Button_zrwjazvut3f", value(approvalCardActionApproveAll)))
	}
	formElements = append(formElements, approvalReasonRow(value(approvalCardActionReject)))
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{"update_multi": true},
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"tag": "plain_text", "content": "权限申请审批"},
			"subtitle": map[string]interface{}{"tag": "plain_text", "content": c.policy.Action},
			"text_tag_list": []interface{}{
				map[string]interface{}{
					"tag":   "text_tag",
					"text":  map[string]interface{}{"tag": "plain_text", "content": "待审批"},
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
					"tag":              "form",
					"elements":         formElements,
					"direction":        "vertical",
					"horizontal_align": "left",
					"vertical_align":   "top",
					"padding":          "4px 0px 4px 0px",
					"margin":           "0px 0px 0px 0px",
					"name":             "Form_msa8n85x",
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

type statusCard struct {
	title    string
	template string
	message  string
}

func (c statusCard) JSON() (string, error) {
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{"update_multi": true},
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"tag": "plain_text", "content": c.title},
			"template": c.template,
		},
		"body": map[string]interface{}{
			"direction": "vertical",
			"elements": []interface{}{
				map[string]interface{}{"tag": "markdown", "content": c.message},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseApprovalCardAction(event *callback.CardActionTriggerEvent) (string, string, bool) {
	if cardActionKind(event) != approvalCardActionKind || event == nil || event.Event == nil || event.Event.Action == nil {
		return "", "", false
	}
	value := event.Event.Action.Value
	requestID := strings.TrimSpace(stringApprovalValue(value, "request_id"))
	action := strings.TrimSpace(stringApprovalValue(value, "action"))
	if requestID == "" || (action != approvalCardActionApproveOnce && action != approvalCardActionApproveAll && action != approvalCardActionReject) {
		return requestID, action, false
	}
	return requestID, action, true
}

func approvalCardReason(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return ""
	}
	return strings.TrimSpace(stringApprovalValue(event.Event.Action.FormValue, "reason"))
}

func stringApprovalValue(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func approvalFormButton(text, buttonType, name string, value map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":   "button",
		"text":  map[string]interface{}{"tag": "plain_text", "content": text},
		"type":  buttonType,
		"width": "default",
		"size":  "medium",
		"behaviors": []interface{}{
			map[string]interface{}{"type": "callback", "value": value},
		},
		"form_action_type": "submit",
		"name":             name,
		"margin":           "4px 0px 4px 0px",
	}
}

func approvalReasonRow(rejectValue map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":                "column_set",
		"horizontal_spacing": "8px",
		"horizontal_align":   "left",
		"columns": []interface{}{
			map[string]interface{}{
				"tag":   "column",
				"width": "auto",
				"elements": []interface{}{
					map[string]interface{}{
						"tag": "input",
						"placeholder": map[string]interface{}{
							"tag":     "plain_text",
							"content": "请输入建议",
						},
						"default_value": "",
						"width":         "default",
						"name":          "reason",
						"margin":        "0px 0px 0px 0px",
					},
				},
				"direction":          "vertical",
				"horizontal_spacing": "8px",
				"vertical_spacing":   "8px",
				"horizontal_align":   "left",
				"vertical_align":     "center",
			},
			map[string]interface{}{
				"tag":              "column",
				"width":            "auto",
				"elements":         []interface{}{approvalFormButton("拒绝", "danger", "Button_k7l2449r9dj", rejectValue)},
				"vertical_spacing": "8px",
				"horizontal_align": "left",
				"vertical_align":   "top",
			},
		},
		"margin": "0px 0px 0px 0px",
	}
}

func approvalCallbackResponse(toastType, content string, card Card) *callback.CardActionTriggerResponse {
	response := cardToast(toastType, content)
	if card == nil {
		return response
	}
	cardJSON, err := card.JSON()
	if err != nil {
		return response
	}
	var cardData map[string]interface{}
	if err := json.Unmarshal([]byte(cardJSON), &cardData); err == nil && cardData != nil {
		response.Card = &callback.Card{Type: "raw", Data: cardData}
	}
	return response
}

func escapeApprovalMarkdown(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"~", "\\~",
		"`", "\\`",
		"[", "\\[",
		"]", "\\]",
		"<", "\\<",
		">", "\\>",
	)
	return replacer.Replace(value)
}

func truncateApprovalRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}
