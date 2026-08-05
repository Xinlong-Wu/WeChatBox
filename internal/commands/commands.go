package commands

import (
	"errors"
	"fmt"
	"strings"

	"lingobridge/internal/session"
	"lingobridge/internal/store"
)

// SessionManager is the session behavior needed by in-chat commands.
type SessionManager interface {
	CurrentSession(userID string) (*store.Session, error)
	CreateSession(userID, name string) (*store.Session, error)
	ListSessions(userID string) ([]store.Session, error)
	SwitchSession(userID, sessionName string) (*store.Session, error)
	RenameCurrentSession(userID, newName string) (*store.Session, error)
	ArchiveSession(userID, sessionName string) (*store.ArchiveResult, error)
	ClearSession(userID string) (*store.Session, error)
	CurrentModel(userID, sessionID string) (string, error)
	SetModel(userID, sessionID, modelName string) error
	DefaultModelName() string
	ListModels() []string
}

// Policy controls which shared slash commands are available for a platform.
type Policy struct {
	Disabled map[string]bool
}

type commandSpec struct {
	Name string
	Help string
}

// ToolSummary is the user-facing capability summary for one platform tool.
type ToolSummary struct {
	Name        string
	Description string
}

// HandleOptions controls command handling behavior for one inbound message.
type HandleOptions struct {
	Policy Policy
	Tools  []ToolSummary
}

var commandSpecs = []commandSpec{
	{Name: "/help", Help: "/help - 查看命令帮助"},
	{Name: "/current", Help: "/current - 查看当前会话和模型"},
	{Name: "/new", Help: "/new [名称] - 创建新会话"},
	{Name: "/list", Help: "/list - 查看会话列表"},
	{Name: "/switch", Help: "/switch <名称> - 切换会话"},
	{Name: "/rename", Help: "/rename <名称> - 重命名当前会话"},
	{Name: "/archive", Help: "/archive [名称] - 归档会话"},
	{Name: "/clear", Help: "/clear - 清空当前会话并开始新会话"},
	{Name: "/model", Help: "/model [名称] - 查看或切换模型"},
	{Name: "/compact", Help: "/compact - 手动压缩当前会话上下文"},
}

func DefaultPolicy() Policy {
	return Policy{}
}

func PolicyWithDisabled(commands ...string) Policy {
	p := Policy{Disabled: map[string]bool{}}
	for _, cmd := range commands {
		cmd = normalizeCommand(cmd)
		if cmd != "" {
			p.Disabled[cmd] = true
		}
	}
	return p
}

func (p Policy) Allows(cmd string) bool {
	cmd = normalizeCommand(cmd)
	if cmd == "" {
		return false
	}
	return !p.Disabled[cmd]
}

func normalizeCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	return cmd
}

// HelpText returns the available in-chat commands for a platform policy.
func HelpText(policy Policy) string {
	return HelpTextWithTools(policy, nil)
}

// HelpTextWithTools returns available commands plus optional platform tool summaries.
func HelpTextWithTools(policy Policy, tools []ToolSummary) string {
	lines := []string{"## 可用命令"}
	for _, spec := range commandSpecs {
		if policy.Allows(spec.Name) {
			lines = append(lines, "- "+markdownCommandHelp(spec.Help))
		}
	}
	toolLines := toolHelpLines(tools)
	if len(toolLines) > 0 {
		lines = append(lines, "", "## 可用工具")
		lines = append(lines, toolLines...)
	}
	return strings.Join(lines, "\n")
}

func markdownCommandHelp(help string) string {
	name, description, ok := strings.Cut(strings.TrimSpace(help), " - ")
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if !ok || name == "" || description == "" {
		return help
	}
	return fmt.Sprintf("`%s` - %s", name, description)
}

// Handle processes a slash command and returns the response text.
// Returns (response, handled, error).
func Handle(text string, userID string, sm SessionManager) (string, bool, error) {
	return HandleWithPolicy(text, userID, sm, DefaultPolicy())
}

// HandleWithPolicy processes a slash command with platform-specific command availability.
func HandleWithPolicy(text string, userID string, sm SessionManager, policy Policy) (string, bool, error) {
	return HandleWithOptions(text, userID, sm, HandleOptions{Policy: policy})
}

// HandleWithOptions processes a slash command with platform command and help options.
func HandleWithOptions(text string, userID string, sm SessionManager, opts HandleOptions) (string, bool, error) {
	text = strings.TrimSpace(text)

	if !strings.HasPrefix(text, "/") {
		return "", false, nil
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", false, nil
	}

	cmd := parts[0]
	args := parts[1:]
	policy := opts.Policy

	if !policy.Allows(cmd) {
		return fmt.Sprintf("此平台暂不支持 %s。", cmd), true, nil
	}

	switch cmd {
	case "/help":
		return handleHelp(policy, opts.Tools)
	case "/current":
		return handleCurrent(userID, sm)
	case "/new":
		return handleNew(userID, args, sm)
	case "/list":
		return handleList(userID, sm)
	case "/switch":
		return handleSwitch(userID, args, sm)
	case "/rename":
		return handleRename(userID, args, sm)
	case "/archive":
		return handleArchive(userID, args, sm)
	case "/clear":
		return handleClear(userID, args, sm)
	case "/model":
		return handleModel(userID, args, sm)
	default:
		return "", false, nil // Not a recognized slash command
	}
}

func handleHelp(policy Policy, tools []ToolSummary) (string, bool, error) {
	return HelpTextWithTools(policy, tools), true, nil
}

func toolHelpLines(tools []ToolSummary) []string {
	lines := []string{}
	seen := map[string]bool{}
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		description := truncateRunes(collapseWhitespace(tool.Description), 120)
		if description == "" {
			lines = append(lines, fmt.Sprintf("- `%s` - 可由当前平台提供给支持工具调用的模型使用", name))
			continue
		}
		lines = append(lines, fmt.Sprintf("- `%s` - %s", name, description))
	}
	return lines
}

func collapseWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func handleCurrent(userID string, sm SessionManager) (string, bool, error) {
	sess, err := sm.CurrentSession(userID)
	if err != nil {
		return "", true, fmt.Errorf("current session: %w", err)
	}
	modelName, err := sm.CurrentModel(userID, sess.ID)
	if err != nil {
		return "", true, fmt.Errorf("current model: %w", err)
	}
	return fmt.Sprintf("当前会话：%s\n当前模型：%s", sess.Name, modelName), true, nil
}

func handleNew(userID string, args []string, sm SessionManager) (string, bool, error) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	sess, err := sm.CreateSession(userID, name)
	if err != nil {
		if errors.Is(err, store.ErrSessionExists) {
			return fmt.Sprintf("❌ 会话 %q 已存在", name), true, nil
		}
		return "", true, fmt.Errorf("create session: %w", err)
	}

	return fmt.Sprintf("✅ 已创建新会话：%s", sess.Name), true, nil
}

func handleList(userID string, sm SessionManager) (string, bool, error) {
	sessions, err := sm.ListSessions(userID)
	if err != nil {
		return "", true, fmt.Errorf("list sessions: %w", err)
	}

	if len(sessions) == 0 {
		return "你还没有任何会话。发送消息即可自动创建默认会话。", true, nil
	}

	return session.FormatSessionList(sessions), true, nil
}

func handleSwitch(userID string, args []string, sm SessionManager) (string, bool, error) {
	if len(args) == 0 {
		return "用法：/switch <会话名称>", true, nil
	}

	name := args[0]
	sess, err := sm.SwitchSession(userID, name)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return fmt.Sprintf("❌ 会话 %q 不存在。使用 /list 查看所有会话。", name), true, nil
		}
		return "", true, fmt.Errorf("switch session: %w", err)
	}

	return fmt.Sprintf("✅ 已切换到会话：%s", sess.Name), true, nil
}

func handleRename(userID string, args []string, sm SessionManager) (string, bool, error) {
	if len(args) == 0 {
		return "用法：/rename <新名称>", true, nil
	}

	name := args[0]
	sess, err := sm.RenameCurrentSession(userID, name)
	if err != nil {
		if errors.Is(err, store.ErrSessionExists) {
			return fmt.Sprintf("❌ 会话 %q 已存在", name), true, nil
		}
		return "", true, fmt.Errorf("rename session: %w", err)
	}
	return fmt.Sprintf("✅ 当前会话已重命名为：%s", sess.Name), true, nil
}

func handleArchive(userID string, args []string, sm SessionManager) (string, bool, error) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	result, err := sm.ArchiveSession(userID, name)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			target := name
			if target == "" {
				target = "当前会话"
			}
			return fmt.Sprintf("❌ 会话 %q 不存在。使用 /list 查看所有会话。", target), true, nil
		}
		return "", true, fmt.Errorf("archive session: %w", err)
	}
	if result.CurrentChanged && result.Current != nil {
		return fmt.Sprintf("✅ 已归档会话：%s\n当前会话：%s", result.Archived.Name, result.Current.Name), true, nil
	}
	return fmt.Sprintf("✅ 已归档会话：%s", result.Archived.Name), true, nil
}

func handleClear(userID string, args []string, sm SessionManager) (string, bool, error) {
	if len(args) > 0 {
		return "用法：/clear", true, nil
	}
	sess, err := sm.ClearSession(userID)
	if err != nil {
		return "", true, fmt.Errorf("clear session: %w", err)
	}
	return fmt.Sprintf("✅ 已清空当前会话，新会话：%s", sess.Name), true, nil
}

func handleModel(userID string, args []string, sm SessionManager) (string, bool, error) {
	sess, err := sm.CurrentSession(userID)
	if err != nil {
		return "", true, fmt.Errorf("current session: %w", err)
	}
	if len(args) == 0 {
		current, err := sm.CurrentModel(userID, sess.ID)
		if err != nil {
			return "", true, fmt.Errorf("current model: %w", err)
		}
		return fmt.Sprintf("当前模型：%s\n默认模型：%s\n可用模型：%s", current, sm.DefaultModelName(), strings.Join(sm.ListModels(), ", ")), true, nil
	}

	modelName := args[0]
	if err := sm.SetModel(userID, sess.ID, modelName); err != nil {
		if errors.Is(err, session.ErrModelNotFound) {
			return fmt.Sprintf("❌ 模型 %q 不存在。可用模型：%s", modelName, strings.Join(sm.ListModels(), ", ")), true, nil
		}
		return "", true, fmt.Errorf("set model: %w", err)
	}
	return fmt.Sprintf("✅ 已切换模型：%s", modelName), true, nil
}
