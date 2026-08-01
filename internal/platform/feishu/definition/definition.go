package definition

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"lingobridge/internal/commands"
	"lingobridge/internal/core"
	"lingobridge/internal/platform"
	"lingobridge/internal/platform/feishu"
	feishumonitor "lingobridge/internal/platform/feishu/monitor"
	"lingobridge/internal/store"
)

const accountNewUsage = "lingobridge account new feishu [--name <name>] [--app-id <id>] [--app-secret <secret>] [--base-url <url>]"

// Definition returns the built-in Feishu platform registration.
func Definition() platform.Definition {
	return platform.Definition{
		ID:              store.PlatformFeishu,
		Aliases:         []string{"飞书"},
		AccountNewUsage: accountNewUsage,
		ParseAccountNewFlags: func(args []string, io platform.AccountNewIO) (platform.AccountNewOptions, error) {
			accountIO := normalizeAccountNewIO(io)
			values, err := feishu.ParseAccountNewFlags(args, accountIO.In, accountIO.Out)
			if err != nil {
				return platform.AccountNewOptions{}, err
			}
			return platform.AccountNewOptions{
				Name:   normalizeAccountName(values.Name),
				Values: values,
			}, nil
		},
		ListAccounts: func(ctx platform.AccountListContext) ([]store.Account, error) {
			cfg, err := feishu.LoadConfig(ctx.Platform)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(cfg.Accounts))
			for name := range cfg.Accounts {
				names = append(names, name)
			}
			sort.Strings(names)
			accounts := make([]store.Account, 0, len(names))
			for _, name := range names {
				accountConfig := cfg.Accounts[name]
				acc, err := feishu.NewAccount(name, accountConfig.AppID, accountConfig.AppSecret, accountConfig.BaseURL)
				if err != nil {
					return nil, fmt.Errorf("platforms.feishu.accounts.%s: %w", name, err)
				}
				accounts = append(accounts, acc)
			}
			return accounts, nil
		},
		CreateOrUpdateAccount: func(ctx platform.AccountNewContext, opts platform.AccountNewOptions) error {
			values, ok := opts.Values.(feishu.AccountNewOptions)
			if !ok {
				return fmt.Errorf("invalid feishu account options")
			}
			acc, err := feishu.NewAccount(opts.Name, values.AppID, values.AppSecret, values.BaseURL)
			if err != nil {
				return err
			}
			accountConfig := feishu.AccountConfig{
				AppID:     values.AppID,
				AppSecret: values.AppSecret,
				BaseURL:   values.BaseURL,
			}
			existing, ok, err := feishu.ResolveAccountConfig(ctx.Platform, opts.Name)
			if err != nil {
				return fmt.Errorf("load existing feishu account config: %w", err)
			}
			if ok {
				accountConfig.OAuthBaseURL = existing.OAuthBaseURL
				accountConfig.OAuthRedirectURI = existing.OAuthRedirectURI
				accountConfig.OAuthListenAddress = existing.OAuthListenAddress
			}
			if err := feishu.UpsertAccountConfig(ctx.Platform, opts.Name, accountConfig); err != nil {
				return err
			}
			fmt.Printf("✅ 已添加飞书账户: %s (%s)\n", acc.Name, acc.ID)
			return nil
		},
		DeleteAccount: func(ctx platform.AccountDeleteContext) error {
			if err := feishu.DeleteAccountConfig(ctx.Platform, ctx.Account.Name); err != nil {
				return err
			}
			st := ctx.Platform.DataStore()
			if err := st.DeleteSyncBuf(ctx.Account.ID); err != nil {
				return fmt.Errorf("delete sync cursor: %w", err)
			}
			if err := st.DeleteToolApprovals(ctx.Account.ID); err != nil {
				return fmt.Errorf("delete tool approvals: %w", err)
			}
			if err := st.DeleteFeishuDocsData(ctx.Account.ID); err != nil {
				return fmt.Errorf("delete feishu docs data: %w", err)
			}
			return nil
		},
		NewRuntimePlatform: func(ctx platform.RuntimeContext) (core.Platform, error) {
			feishuConfig, err := feishu.LoadConfig(ctx.Platform)
			if err != nil {
				return nil, err
			}
			return feishumonitor.NewPlatform(ctx.Store, ctx.Account, feishuConfig, ctx.LogLevel), nil
		},
		CommandPolicy:       commands.DefaultPolicy(),
		TextChunkLimit:      feishu.TextChunkLimit,
		EnableTextStreaming: true,
	}
}

func normalizeAccountName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default"
	}
	return name
}

func normalizeAccountNewIO(io platform.AccountNewIO) platform.AccountNewIO {
	if io.In == nil {
		io.In = os.Stdin
	}
	if io.Out == nil {
		io.Out = os.Stdout
	}
	return io
}
