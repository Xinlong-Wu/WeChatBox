package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lingobridge/internal/config"
	"lingobridge/internal/control"
	"lingobridge/internal/core"
	"lingobridge/internal/logging"
	"lingobridge/internal/mcp"
	"lingobridge/internal/platform"
	"lingobridge/internal/platform/builtins"
	"lingobridge/internal/runner"
	"lingobridge/internal/session"
	"lingobridge/internal/store"
)

var ErrUsage = errors.New("usage")

var (
	cliLog     = logging.For("cli")
	configLog  = logging.For("config")
	controlLog = logging.For("control")
	runtimeLog = logging.For("runtime")
)

func logBuildInfo() {
	ctx := context.Background()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		cliLog.Info(ctx, "LingoBridge dev go/%s", runtime.Version()[2:])
		return
	}

	version := info.Main.Version
	if version == "" || version == "(devel)" {
		// Try VCS info
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				version = s.Value
				if len(version) > 8 {
					version = version[:8]
				}
			case "vcs.modified":
				if s.Value == "true" {
					version += "-dirty"
				}
			}
		}
	}
	if version == "" || version == "(devel)" {
		version = "dev"
	}

	var vcsTime string
	for _, s := range info.Settings {
		if s.Key == "vcs.time" {
			vcsTime = s.Value
			break
		}
	}

	if vcsTime != "" {
		cliLog.Info(ctx, "LingoBridge %s (%s) go/%s", version, vcsTime, runtime.Version()[2:])
	} else {
		cliLog.Info(ctx, "LingoBridge %s go/%s", version, runtime.Version()[2:])
	}
}

func Run(args []string) error {
	if len(args) < 1 {
		printUsage()
		return ErrUsage
	}
	if commandNeedsConfig(args) {
		if err := ensureConfigInitialized(os.Stdin, os.Stdout); err != nil {
			return err
		}
	}

	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "account":
		return cmdAccount(args[1:])
	case "model":
		return cmdModel(args[1:])
	default:
		printUsage()
		return ErrUsage
	}
}

func commandNeedsConfig(args []string) bool {
	switch args[0] {
	case "run":
		return true
	case "account":
		if len(args) < 2 {
			return false
		}
		switch args[1] {
		case "new", "list", "delete":
			return true
		default:
			return false
		}
	case "model":
		return len(args) >= 2 && args[1] == "add"
	default:
		return false
	}
}

func cmdAccount(args []string) error {
	if len(args) < 1 {
		printAccountUsage()
		return ErrUsage
	}

	switch args[0] {
	case "new":
		return cmdAccountNew(args[1:])
	case "list":
		return cmdAccountList(args[1:])
	case "delete":
		return cmdAccountDelete(args[1:])
	default:
		printAccountUsage()
		return ErrUsage
	}
}

func cmdModel(args []string) error {
	if len(args) < 1 {
		printModelUsage()
		return ErrUsage
	}

	switch args[0] {
	case "add":
		return cmdModelAdd(args[1:], os.Stdin, os.Stdout)
	default:
		printModelUsage()
		return ErrUsage
	}
}

func printUsage() {
	fmt.Println("LingoBridge - WeChat/Feishu/GitHub Bot → LLM Direct Bridge")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  lingobridge account new <weixin|feishu|github> [platform options]")
	fmt.Println("                                           Add a bot account")
	fmt.Println("  lingobridge account list                   List all accounts")
	fmt.Println("  lingobridge account delete <name|platform/name>")
	fmt.Println("                                           Delete an account")
	fmt.Println("  lingobridge model add <name> [model options]")
	fmt.Println("                                           Add an LLM model profile")
	fmt.Println("  lingobridge run [--account <name>] [--verbose <all|debug|info|warn|error>]")
	fmt.Println("                                           Start the bot loop")
}

func printAccountUsage() {
	fmt.Println("Usage:")
	fmt.Println("  lingobridge account new <weixin|feishu|github> [platform options]")
	fmt.Println("  lingobridge account list")
	fmt.Println("  lingobridge account delete <name|platform/name>")
}

func printModelUsage() {
	fmt.Println("Usage:")
	fmt.Println("  lingobridge model add <name> [--provider <openai|anthropic>] [--base-url <url>] [--api-key <key>] [--id <model-id>] [--endpoint <mode>] [--context-window <tokens>] [--compact <true|false|auto>] [--compact-threshold <ratio>] [--compact-instructions <text>] [--default]")
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func openStore(platformID string) (*store.Store, error) {
	st, err := store.Open(platformID)
	if err != nil {
		return nil, fmt.Errorf("open %s store: %w", platformID, err)
	}
	return st, nil
}

type accountCatalog struct {
	registry *platform.Registry
	stores   map[string]*store.Store
	mu       sync.RWMutex
	cfg      config.Config
}

func openAccountCatalog(registry *platform.Registry, cfg config.Config) (*accountCatalog, error) {
	catalog := &accountCatalog{
		registry: registry,
		stores:   map[string]*store.Store{},
		cfg:      cfg,
	}
	for _, platformID := range registry.PlatformNames() {
		st, err := openStore(platformID)
		if err != nil {
			catalog.Close()
			return nil, err
		}
		catalog.stores[platformID] = st
	}
	return catalog, nil
}

func (c *accountCatalog) Close() {
	for _, st := range c.stores {
		_ = st.Close()
	}
}

func (c *accountCatalog) UpdateConfig(cfg config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
}

func (c *accountCatalog) configSnapshot() config.Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *accountCatalog) platformContext(platformID string, saveConfig func(config.Config) error) (*core.PlatformContext, error) {
	st, ok := c.stores[platformID]
	if !ok {
		return nil, fmt.Errorf("store for platform %q is not open", platformID)
	}
	cfg := c.configSnapshot()
	return core.NewPlatformContext(platformID, &cfg, st, saveConfig)
}

func (c *accountCatalog) ListAccounts() ([]store.Account, error) {
	accounts := make([]store.Account, 0)
	for _, platformID := range c.registry.PlatformNames() {
		def, ok := c.registry.LookupAccountPlatform(platformID)
		if !ok {
			return nil, fmt.Errorf("unsupported account platform %q", platformID)
		}
		platformCtx, err := c.platformContext(platformID, nil)
		if err != nil {
			return nil, err
		}
		platformAccounts, err := def.ListAccounts(platform.AccountListContext{Platform: platformCtx})
		if err != nil {
			return nil, fmt.Errorf("list %s accounts: %w", platformID, err)
		}
		runtimeLog.Debug(context.Background(), "listed platform accounts platform=%s count=%d", platformID, len(platformAccounts))
		accounts = append(accounts, platformAccounts...)
	}
	return accounts, nil
}

func accountDisplayName(a store.Account) string {
	return a.Platform + "/" + a.Name
}

func sortedAccounts(accounts []store.Account) []store.Account {
	out := append([]store.Account(nil), accounts...)
	sort.Slice(out, func(i, j int) bool {
		left := accountDisplayName(out[i])
		right := accountDisplayName(out[j])
		if left != right {
			return left < right
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func resolveAccountSelector(selector string, accounts []store.Account, registry *platform.Registry) (store.Account, error) {
	selector = strings.TrimSpace(selector)
	if platformName, accountName, ok := strings.Cut(selector, "/"); ok {
		def, ok := registry.Lookup(platformName)
		if !ok {
			return store.Account{}, fmt.Errorf("unsupported platform %q; use one of: %s", platformName, strings.Join(registry.PlatformNames(), ", "))
		}
		matches := make([]store.Account, 0, 1)
		for _, a := range accounts {
			if a.Platform == def.ID && a.Name == accountName {
				matches = append(matches, a)
			}
		}
		return resolveAccountMatches(selector, matches)
	}

	matches := make([]store.Account, 0, 1)
	for _, a := range accounts {
		if a.Name == selector {
			matches = append(matches, a)
		}
	}
	return resolveAccountMatches(selector, matches)
}

func resolveAccountMatches(selector string, matches []store.Account) (store.Account, error) {
	switch len(matches) {
	case 0:
		return store.Account{}, fmt.Errorf("account %q not found", selector)
	case 1:
		return matches[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "account %q is ambiguous; specify platform/name:", selector)
		for _, a := range sortedAccounts(matches) {
			fmt.Fprintf(&b, "\n  - %s", accountDisplayName(a))
		}
		return store.Account{}, errors.New(b.String())
	}
}

func ensureConfigInitialized(in io.Reader, out io.Writer) error {
	if _, err := config.Load(); err == nil {
		return nil
	} else if !errors.Is(err, config.ErrConfigNotFound) {
		return fmt.Errorf("load config: %w", err)
	}

	fmt.Fprintln(out, "未找到 config.yaml，开始创建初始配置。")
	cfg := config.DefaultConfig()
	name, model, err := promptModelProfile("", config.LLMModelConfig{}, in, out)
	if err != nil {
		return fmt.Errorf("initialize config: %w", err)
	}
	if err := config.AddModel(&cfg, name, model, true); err != nil {
		return fmt.Errorf("initialize config: %w", err)
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	path, _ := config.ConfigPath()
	fmt.Fprintf(out, "已创建配置文件：%s\n", path)
	return nil
}

func cmdModelAdd(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		printModelUsage()
		return ErrUsage
	}
	name := args[0]
	fs := newFlagSet("model add")
	provider := fs.String("provider", "", "model provider")
	baseURL := fs.String("base-url", "", "model API base URL")
	apiKey := fs.String("api-key", "", "model API key")
	modelID := fs.String("id", "", "provider model ID")
	endpoint := fs.String("endpoint", "", "endpoint mode")
	contextWindow := fs.Int("context-window", 0, "model context window in tokens")
	compactMode := fs.String("compact", "", "compact mode: true, false, or auto")
	compactThreshold := fs.Float64("compact-threshold", 0, "compact threshold ratio")
	compactInstructions := fs.String("compact-instructions", "", "compact instructions")
	makeDefault := fs.Bool("default", false, "set as default model")
	if err := fs.Parse(args[1:]); err != nil {
		printModelUsage()
		return ErrUsage
	}
	if fs.NArg() > 0 {
		printModelUsage()
		return ErrUsage
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.LLM.HasModel(name) {
		return fmt.Errorf("%w: %s", config.ErrModelExists, name)
	}
	model := config.LLMModelConfig{
		Provider:      *provider,
		BaseURL:       *baseURL,
		APIKey:        *apiKey,
		ID:            *modelID,
		Endpoint:      *endpoint,
		ContextWindow: *contextWindow,
		Compact: config.LLMCompactConfig{
			Threshold:    *compactThreshold,
			Instructions: *compactInstructions,
		},
	}
	if strings.TrimSpace(*compactMode) != "" {
		mode, err := config.ParseCompactMode(*compactMode)
		if err != nil {
			return err
		}
		model.Compact.Mode = mode
	}
	name, model, err = promptModelProfile(name, model, in, out)
	if err != nil {
		return err
	}
	if err := config.AddModel(&cfg, name, model, *makeDefault); err != nil {
		return err
	}
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if *makeDefault || cfg.LLM.DefaultModel == name {
		fmt.Fprintf(out, "✅ 已添加模型: %s (default)\n", name)
	} else {
		fmt.Fprintf(out, "✅ 已添加模型: %s\n", name)
	}
	notifyRunningProcess()
	return nil
}

func promptModelProfile(name string, model config.LLMModelConfig, in io.Reader, out io.Writer) (string, config.LLMModelConfig, error) {
	reader := bufio.NewReader(in)
	var err error
	if strings.TrimSpace(name) == "" {
		name, err = promptCLIValue(reader, out, "模型名称: ", true, "")
		if err != nil {
			return "", model, err
		}
	}
	if strings.TrimSpace(model.Provider) == "" {
		model.Provider, err = promptCLIValue(reader, out, "Provider (openai/anthropic，默认 openai): ", true, "openai")
		if err != nil {
			return "", model, err
		}
	}
	if strings.TrimSpace(model.BaseURL) == "" {
		model.BaseURL, err = promptCLIValue(reader, out, "API Base URL: ", true, "")
		if err != nil {
			return "", model, err
		}
	}
	if strings.TrimSpace(model.APIKey) == "" {
		model.APIKey, err = promptCLIValue(reader, out, "API Key: ", true, "")
		if err != nil {
			return "", model, err
		}
	}
	if strings.TrimSpace(model.ID) == "" {
		model.ID, err = promptCLIValue(reader, out, "模型 ID: ", true, "")
		if err != nil {
			return "", model, err
		}
	}
	if strings.TrimSpace(model.Endpoint) == "" {
		defaultEndpoint := config.DefaultEndpointForProvider(strings.TrimSpace(model.Provider))
		model.Endpoint, err = promptEndpoint(reader, out, strings.TrimSpace(model.Provider), defaultEndpoint)
		if err != nil {
			return "", model, err
		}
	}
	if model.ContextWindow <= 0 && compactCanAutoUseEndpoint(model.Provider, model.Endpoint, model.Compact.Mode) {
		model.ContextWindow, err = promptCLIInt(reader, out, "上下文窗口 tokens（compact auto 需要）: ", true, 0)
		if err != nil {
			return "", model, err
		}
	}
	return strings.TrimSpace(name), model, nil
}

func compactCanAutoUseEndpoint(provider, endpoint string, mode config.CompactMode) bool {
	if mode == "" {
		mode = config.CompactModeAuto
	}
	return mode != config.CompactModeFalse && config.SupportsNativeCompact(strings.TrimSpace(provider), strings.TrimSpace(endpoint))
}

func promptEndpoint(reader *bufio.Reader, out io.Writer, provider, defaultEndpoint string) (string, error) {
	choices := strings.Join(config.EndpointChoicesForProvider(provider), "/")
	prompt := fmt.Sprintf("Endpoint（%s；直接回车使用 %s。OpenAI 图像/Responses API 请填 responses，注意不是 response）: ", choices, defaultEndpoint)
	for {
		endpoint, err := promptCLIValue(reader, out, prompt, false, defaultEndpoint)
		if err != nil {
			return "", err
		}
		endpoint = strings.TrimSpace(endpoint)
		if config.IsValidEndpointForProvider(provider, endpoint) {
			return endpoint, nil
		}
		fmt.Fprintf(out, "Endpoint 无效：%s。%s 可用值：%s。\n", endpoint, provider, choices)
		if strings.EqualFold(endpoint, "response") {
			fmt.Fprintln(out, "提示：OpenAI Responses endpoint 使用复数 responses。")
		}
	}
}

func promptCLIValue(reader *bufio.Reader, out io.Writer, prompt string, required bool, defaultValue string) (string, error) {
	for {
		fmt.Fprint(out, prompt)
		value, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			value = defaultValue
		}
		if value != "" || !required {
			return value, nil
		}
		fmt.Fprintln(out, "此项必填，请重新输入。")
		if err == io.EOF {
			return "", fmt.Errorf("missing required input")
		}
	}
}

func promptCLIInt(reader *bufio.Reader, out io.Writer, prompt string, required bool, defaultValue int) (int, error) {
	defaultText := ""
	if defaultValue > 0 {
		defaultText = strconv.Itoa(defaultValue)
	}
	for {
		value, err := promptCLIValue(reader, out, prompt, required, defaultText)
		if err != nil {
			return 0, err
		}
		if strings.TrimSpace(value) == "" && !required {
			return 0, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && n > 0 {
			return n, nil
		}
		fmt.Fprintln(out, "请输入大于 0 的整数。")
	}
}

type runtimeState struct {
	stores   map[string]*store.Store
	registry *platform.Registry
	mcpHost  *mcp.Host
	mu       sync.RWMutex
	cfg      config.Config
	runtimes map[string]platformRuntime
	digest   string
}

type platformRuntime struct {
	store   *store.Store
	sm      *session.Manager
	handler *core.Bot
}

func newRuntimeState(stores map[string]*store.Store, cfg config.Config) (*runtimeState, error) {
	registry, err := builtins.NewRegistry()
	if err != nil {
		return nil, err
	}
	rs := &runtimeState{stores: stores, registry: registry, mcpHost: mcp.NewHost()}
	if err := rs.updateConfig(context.Background(), cfg); err != nil {
		return nil, err
	}
	return rs, nil
}

func (r *runtimeState) updateConfig(ctx context.Context, cfg config.Config) error {
	if err := cfg.LLM.Validate(); err != nil {
		return fmt.Errorf("validate llm config: %w", err)
	}
	if r.mcpHost != nil {
		if err := r.mcpHost.Reload(ctx, cfg.MCP); err != nil {
			return fmt.Errorf("reload mcp config: %w", err)
		}
	}
	runtimes := make(map[string]platformRuntime, len(r.stores))
	for platformID, st := range r.stores {
		resetCount, err := st.ResetUnavailableSessionModels(cfg.LLM.DefaultModel, cfg.LLM.ModelNames())
		if err != nil {
			return fmt.Errorf("reset %s session models: %w", platformID, err)
		}
		if resetCount > 0 {
			runtimeLog.Info(context.Background(), "reset %d %s session model preference(s) to default model %q", resetCount, platformID, cfg.LLM.DefaultModel)
		}
		sm := session.NewManager(st, cfg.LLM)
		handler := core.New(sm, cfg.LLM)
		def, ok := r.registry.LookupAccountPlatform(platformID)
		if !ok {
			return fmt.Errorf("unsupported account platform %q", platformID)
		}
		handler.TextChunkLimit = def.TextChunkLimit
		handler.EnableTextStreaming = def.EnableTextStreaming
		handler.ToolProvider = r.mcpHost
		runtimes[platformID] = platformRuntime{
			store:   st,
			sm:      sm,
			handler: handler,
		}
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg = cfg
	r.runtimes = runtimes
	r.digest = digest
	r.mu.Unlock()
	logConfig(cfg)
	return nil
}

func (r *runtimeState) Close(ctx context.Context) error {
	if r == nil || r.mcpHost == nil {
		return nil
	}
	return r.mcpHost.Close(ctx)
}

func (r *runtimeState) snapshot(platformID string) (config.Config, platformRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.runtimes[platformID]
	return r.cfg, rt, ok
}

func (r *runtimeState) signatureExtra(acc store.Account) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if acc.Platform == store.PlatformFeishu {
		return r.digest
	}
	return r.digest
}

func logConfig(cfg config.Config) {
	ctx := context.Background()
	configLog.Info(ctx, "llm default_model: %s", cfg.LLM.DefaultModel)
	configLog.Info(ctx, "llm models: %s", strings.Join(cfg.LLM.ModelNames(), ", "))
	configLog.Info(ctx, "llm max_history: %d", cfg.LLM.MaxHistory)
	configLog.Info(ctx, "llm system_prompt: %s", cfg.LLM.SystemPrompt)
	configLog.Info(ctx, "mcp servers: %s", strings.Join(cfg.MCP.ServerNames(), ", "))
}

func cmdAccountNew(args []string) error {
	registry, err := builtins.NewRegistry()
	if err != nil {
		return err
	}
	return cmdAccountNewWithRegistry(args, registry)
}

func cmdAccountNewWithRegistry(args []string, registry *platform.Registry) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Println("Usage: lingobridge account new <weixin|feishu|github> [platform options]")
		return ErrUsage
	}
	def, ok := registry.Lookup(args[0])
	if !ok {
		return fmt.Errorf("unsupported platform %q; use one of: %s", args[0], strings.Join(registry.PlatformNames(), ", "))
	}
	opts, err := def.ParseAccountNewFlags(args[1:], platform.DefaultAccountNewIO())
	if err != nil {
		fmt.Println("Usage:", def.AccountNewUsage)
		return ErrUsage
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	catalog, err := openAccountCatalog(registry, cfg)
	if err != nil {
		return err
	}
	defer catalog.Close()

	accounts, err := catalog.ListAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	exists := func(name string) bool {
		for _, a := range accounts {
			if a.Name == name {
				return true
			}
		}
		return false
	}

	originalName := opts.Name
	suffix := 2
	for exists(opts.Name) {
		opts.Name = fmt.Sprintf("%s-%d", originalName, suffix)
		suffix++
	}
	if opts.Name != originalName {
		fmt.Printf("Name %q already exists, using %q instead.\n", originalName, opts.Name)
	}

	platformCtx, err := catalog.platformContext(def.ID, config.Save)
	if err != nil {
		return err
	}
	if err := def.CreateOrUpdateAccount(platform.AccountNewContext{Platform: platformCtx}, opts); err != nil {
		return err
	}
	notifyRunningProcess()
	return nil
}

type runOptions struct {
	targetAccount string
	logLevel      logging.Level
}

func parseRunOptions(args []string) (runOptions, error) {
	fs := newFlagSet("run")
	targetAccount := fs.String("account", "", "account name")
	verbose := fs.String("verbose", logging.Info.String(), "log level: all, debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return runOptions{}, ErrUsage
	}
	logLevel, err := logging.ParseLevel(*verbose)
	if err != nil {
		return runOptions{}, err
	}
	return runOptions{targetAccount: *targetAccount, logLevel: logLevel}, nil
}

func cmdRun(args []string) error {
	opts, err := parseRunOptions(args)
	if err != nil {
		fmt.Println("Usage: lingobridge run [--account <name>] [--verbose <all|debug|info|warn|error>]")
		return err
	}
	logging.SetLevel(opts.logLevel)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logBuildInfo()

	registry, err := builtins.NewRegistry()
	if err != nil {
		return err
	}
	catalog, err := openAccountCatalog(registry, cfg)
	if err != nil {
		return err
	}
	defer catalog.Close()

	state, err := newRuntimeState(catalog.stores, cfg)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := state.Close(shutdownCtx); err != nil {
			runtimeLog.Warn(shutdownCtx, "mcp host shutdown: %v", err)
		}
	}()

	runCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	fatalMonitorErr := make(chan error, 1)

	supervisor := runner.NewSupervisor(catalog, func(ctx context.Context, acc store.Account) error {
		cfg, rt, ok := state.snapshot(acc.Platform)
		if !ok {
			return fmt.Errorf("runtime for platform %q is not available", acc.Platform)
		}
		def, ok := registry.LookupAccountPlatform(acc.Platform)
		if !ok {
			return fmt.Errorf("unsupported account platform %q for account %s", acc.Platform, acc.Name)
		}
		platformCtx, err := core.NewPlatformContext(acc.Platform, &cfg, rt.store, nil)
		if err != nil {
			return err
		}
		runtimePlatform, err := def.RuntimePlatform(platform.RuntimeContext{
			Store:     rt.store,
			Sessions:  rt.sm,
			Platform:  platformCtx,
			Config:    cfg,
			LLMConfig: cfg.LLM,
			Account:   acc,
			LogLevel:  opts.logLevel,
		})
		if err != nil {
			return err
		}
		return runtimePlatform.Run(ctx, rt.handler)
	}, opts.targetAccount)
	supervisor.SetSignatureExtra(state.signatureExtra)
	supervisor.SetMonitorExitHandler(func(exit runner.MonitorExit) {
		if exit.RemainingRunning != 0 {
			return
		}
		select {
		case fatalMonitorErr <- formatMonitorExitError(exit):
		default:
		}
	})

	controlServer, err := control.StartServer(runCtx, func(ctx context.Context) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := state.updateConfig(ctx, cfg); err != nil {
			return err
		}
		catalog.UpdateConfig(cfg)
		return supervisor.Reconcile(runCtx)
	})
	if err != nil {
		if errors.Is(err, control.ErrAlreadyRunning) {
			return fmt.Errorf("another lingobridge run process is already active")
		}
		return fmt.Errorf("start control server: %w", err)
	}

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := controlServer.Close(shutdownCtx); err != nil {
			controlLog.Warn(shutdownCtx, "server shutdown: %v", err)
		}
	}()

	if err := supervisor.Reconcile(runCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("reconcile accounts: %w", err)
	}

	runtimeLog.Info(runCtx, "listening on %d account(s)...", supervisor.RunningCount())
	err = waitRunDone(runCtx, fatalMonitorErr)
	runtimeLog.Info(runCtx, "shutting down...")
	supervisor.Stop()
	return err
}

func waitRunDone(ctx context.Context, fatalMonitorErr <-chan error) error {
	select {
	case err := <-fatalMonitorErr:
		return err
	case <-ctx.Done():
		return nil
	}
}

func formatMonitorExitError(exit runner.MonitorExit) error {
	return fmt.Errorf("monitor exited platform=%s name=%s id=%s: %w", exit.Account.Platform, exit.Account.Name, exit.Account.ID, exit.Err)
}

func cmdAccountList(args []string) error {
	registry, err := builtins.NewRegistry()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	catalog, err := openAccountCatalog(registry, cfg)
	if err != nil {
		return err
	}
	defer catalog.Close()

	accounts, err := catalog.ListAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	if len(accounts) == 0 {
		fmt.Println("No accounts. Run 'lingobridge account new' to add one.")
		return nil
	}

	fmt.Println("Accounts:")
	for _, a := range sortedAccounts(accounts) {
		status := "✓"
		if !a.Enabled {
			status = "✗"
		}
		fmt.Printf("  %s %s (id: %s)\n", status, accountDisplayName(a), a.ID)
	}
	return nil
}

func cmdAccountDelete(args []string) error {
	if len(args) < 1 {
		fmt.Println("Usage: lingobridge account delete <name|platform/name>")
		return ErrUsage
	}
	selector := strings.TrimSpace(strings.Join(args, " "))

	registry, err := builtins.NewRegistry()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	catalog, err := openAccountCatalog(registry, cfg)
	if err != nil {
		return err
	}
	defer catalog.Close()

	accounts, err := catalog.ListAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	target, err := resolveAccountSelector(selector, accounts, registry)
	if err != nil {
		return err
	}

	def, ok := registry.LookupAccountPlatform(target.Platform)
	if !ok {
		return fmt.Errorf("unsupported account platform %q", target.Platform)
	}
	platformCtx, err := catalog.platformContext(target.Platform, config.Save)
	if err != nil {
		return err
	}
	if err := def.DeleteAccount(platform.AccountDeleteContext{Platform: platformCtx, Account: target}); err != nil {
		return fmt.Errorf("delete %s account: %w", target.Platform, err)
	}

	fmt.Printf("Deleted account: %s\n", accountDisplayName(target))
	notifyRunningProcess()
	return nil
}

func notifyRunningProcess() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := control.NotifyReload(ctx)
	switch {
	case err == nil:
		fmt.Println("Reloaded running lingobridge process.")
	case errors.Is(err, control.ErrUnavailable):
		fmt.Println("Note: No running lingobridge process found; start or restart 'lingobridge run' to pick up changes.")
	default:
		fmt.Printf("Warning: notify running process failed: %v\n", err)
	}
}
