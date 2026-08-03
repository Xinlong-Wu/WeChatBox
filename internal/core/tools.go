package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"lingobridge/internal/commands"
	"lingobridge/internal/store"
	tooltypes "lingobridge/internal/tools"
)

const (
	defaultMaxToolCalls       = 5
	defaultToolTimeout        = 15 * time.Second
	defaultToolResultLimit    = 12000
	defaultToolTraceTextLimit = 1024
)

type toolBudgetReminder int

const (
	toolBudgetReminderNone toolBudgetReminder = iota
	toolBudgetReminderTenPercent
	toolBudgetReminderFivePercent
)

func mergeTools(ctx context.Context, platformTools, providerTools []tooltypes.Tool) []tooltypes.Tool {
	if len(platformTools) == 0 && len(providerTools) == 0 {
		return nil
	}
	tools := make([]tooltypes.Tool, 0, len(platformTools)+len(providerTools))
	seen := map[string]string{}
	add := func(source string, candidates []tooltypes.Tool) {
		for _, tool := range candidates {
			name := toolName(tool)
			if name == "" {
				continue
			}
			if firstSource, ok := seen[name]; ok {
				coreLog.Warn(ctx, "skipping duplicate tool name=%s source=%s first_source=%s", name, source, firstSource)
				continue
			}
			seen[name] = source
			tools = append(tools, tool)
		}
	}
	add("platform", platformTools)
	add("provider", providerTools)
	return tools
}

func toolName(tool tooltypes.Tool) string {
	if tool == nil {
		return ""
	}
	return strings.TrimSpace(tool.Spec().Name)
}

func toolSpecs(tools []tooltypes.Tool) []tooltypes.Spec {
	specs := make([]tooltypes.Spec, 0, len(tools))
	for _, tool := range tools {
		spec := toolSpec(tool)
		if spec.Name == "" {
			continue
		}
		specs = append(specs, spec)
	}
	return specs
}

func toolMap(tools []tooltypes.Tool) map[string]tooltypes.Tool {
	out := map[string]tooltypes.Tool{}
	for _, tool := range tools {
		name := toolName(tool)
		if name != "" {
			out[name] = tool
		}
	}
	return out
}

func commandToolSummaries(tools []tooltypes.Tool) []commands.ToolSummary {
	summaries := make([]commands.ToolSummary, 0, len(tools))
	for _, tool := range tools {
		spec := toolSpec(tool)
		if spec.Name == "" {
			continue
		}
		summaries = append(summaries, commands.ToolSummary{
			Name:        spec.Name,
			Description: spec.Description,
		})
	}
	return summaries
}

func mergeToolOptions(base, override tooltypes.Options) tooltypes.Options {
	if override.MaxCalls > 0 {
		base.MaxCalls = override.MaxCalls
	}
	if override.Timeout > 0 {
		base.Timeout = override.Timeout
	}
	if override.ResultLimit > 0 {
		base.ResultLimit = override.ResultLimit
	}
	return base
}

func toolBudgetSystemPrompt(systemPrompt string, maxCalls int, reminder toolBudgetReminder, remaining int) string {
	var sections []string
	if base := strings.TrimSpace(systemPrompt); base != "" {
		sections = append(sections, base)
	}
	sections = append(sections, fmt.Sprintf(`<tool_call_budget>
You may call tools at most %d times in this tool loop. Plan before calling tools, prioritize high-value calls, and avoid repeating failed or low-value tool calls.
</tool_call_budget>`, maxCalls))
	switch reminder {
	case toolBudgetReminderTenPercent:
		sections = append(sections, fmt.Sprintf(`<tool_call_budget_reminder severity="10%%" remaining="%d" max_calls="%d">
Only %d of %d tool calls remain. Stop exploratory tool calls, prioritize the minimum necessary reads/actions, and prepare to finish.
</tool_call_budget_reminder>`, remaining, maxCalls, remaining, maxCalls))
	case toolBudgetReminderFivePercent:
		sections = append(sections, fmt.Sprintf(`<tool_call_budget_reminder severity="5%%" remaining="%d" max_calls="%d">
Only %d of %d tool calls remain. Do not make non-essential tool calls; immediately complete the required final action or final answer.
</tool_call_budget_reminder>`, remaining, maxCalls, remaining, maxCalls))
	}
	return strings.Join(sections, "\n\n")
}

func nextToolBudgetReminder(maxCalls, remaining int, sentTenPercent, sentFivePercent bool) toolBudgetReminder {
	if remaining <= toolBudgetFivePercentThreshold(maxCalls) && !sentFivePercent {
		return toolBudgetReminderFivePercent
	}
	if remaining <= toolBudgetTenPercentThreshold(maxCalls) && !sentTenPercent {
		return toolBudgetReminderTenPercent
	}
	return toolBudgetReminderNone
}

func toolBudgetTenPercentThreshold(maxCalls int) int {
	return ceilDiv(maxCalls, 10)
}

func toolBudgetFivePercentThreshold(maxCalls int) int {
	return ceilDiv(maxCalls, 20)
}

func ceilDiv(n, d int) int {
	if n <= 0 {
		return 1
	}
	if d <= 0 {
		return n
	}
	return (n + d - 1) / d
}

func toolSpec(tool tooltypes.Tool) tooltypes.Spec {
	if tool == nil {
		return tooltypes.Spec{}
	}
	spec := tool.Spec()
	spec.Name = strings.TrimSpace(spec.Name)
	return spec
}

func commandName(text string) string {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func runTool(ctx context.Context, tool tooltypes.Tool, call tooltypes.Call, timeout time.Duration, resultLimit int) (tooltypes.Result, store.ToolTrace, error) {
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	if resultLimit <= 0 {
		resultLimit = defaultToolResultLimit
	}

	start := time.Now()
	trace := store.ToolTrace{
		CallID:    call.ID,
		Name:      call.Name,
		Status:    "ok",
		Arguments: summarizeJSON(call.Arguments, defaultToolTraceTextLimit),
	}

	if err := ctx.Err(); err != nil {
		result := tooltypes.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Content: err.Error(),
			IsError: true,
		}
		trace.Status = "error"
		trace.Error = result.Content
		trace.DurationMillis = time.Since(start).Milliseconds()
		return result, trace, err
	}

	if tool == nil {
		result := tooltypes.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Content: fmt.Sprintf("tool %q is not available", call.Name),
			IsError: true,
		}
		trace.Status = "error"
		trace.Error = result.Content
		trace.DurationMillis = time.Since(start).Milliseconds()
		return result, trace, nil
	}

	resolvedToolName := toolName(tool)
	if resolvedToolName == "" {
		resolvedToolName = call.Name
	}
	executionCtx := tooltypes.WithToolCall(ctx, call.ID, resolvedToolName)
	toolCtx, cancel := context.WithTimeout(executionCtx, timeout)
	defer cancel()

	done := make(chan tooltypes.Result, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- tooltypes.Result{
					CallID:  call.ID,
					Name:    call.Name,
					Content: fmt.Sprintf("tool panicked: %v", recovered),
					IsError: true,
				}
			}
		}()
		result := tool.Execute(toolCtx, call)
		if result.CallID == "" {
			result.CallID = call.ID
		}
		if result.Name == "" {
			result.Name = call.Name
		}
		done <- result
	}()

	var result tooltypes.Result
	select {
	case result = <-done:
		if err := ctx.Err(); err != nil {
			result = tooltypes.Result{
				CallID:            call.ID,
				Name:              call.Name,
				Content:           err.Error(),
				IsError:           true,
				PendingWorkflowID: result.PendingWorkflowID,
			}
			trace.Status = "error"
			trace.Error = result.Content
			trace.DurationMillis = time.Since(start).Milliseconds()
			return result, trace, err
		}
	case <-toolCtx.Done():
		if err := ctx.Err(); err != nil {
			result = tooltypes.Result{
				CallID:  call.ID,
				Name:    call.Name,
				Content: err.Error(),
				IsError: true,
			}
			trace.Status = "error"
			trace.Error = result.Content
			trace.DurationMillis = time.Since(start).Milliseconds()
			return result, trace, err
		}
		result = tooltypes.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Content: fmt.Sprintf("tool timed out after %s", timeout),
			IsError: true,
		}
	}

	result.Content = truncateText(result.Content, resultLimit)
	if result.IsError {
		trace.Status = "error"
		trace.Error = truncateText(result.Content, defaultToolTraceTextLimit)
	} else {
		trace.Result = truncateText(result.Content, defaultToolTraceTextLimit)
	}
	trace.DurationMillis = time.Since(start).Milliseconds()
	return result, trace, nil
}

func summarizeJSON(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if normalized, err := json.Marshal(v); err == nil {
			return truncateText(string(normalized), limit)
		}
	}
	return truncateText(string(raw), limit)
}

func summarizeToolArgumentsForLog(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		v = redactToolLogValue(v)
		if normalized, err := json.Marshal(v); err == nil {
			return truncateText(string(normalized), limit)
		}
	}
	return truncateText(string(raw), limit)
}

func redactToolLogValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if sensitiveToolArgumentKey(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactToolLogValue(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = redactToolLogValue(child)
		}
		return out
	default:
		return value
	}
}

func sensitiveToolArgumentKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, pattern := range []string{"token", "secret", "password", "api_key", "apikey", "authorization", "auth"} {
		if strings.Contains(key, pattern) {
			return true
		}
	}
	return false
}

func truncateText(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	if limit <= 16 {
		return string(runes[:limit])
	}
	return string(runes[:limit-14]) + "\n[truncated]"
}
