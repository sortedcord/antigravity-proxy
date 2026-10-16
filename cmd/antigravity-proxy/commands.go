package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"github.com/terastal-hq/proxylib/accounts"
	"github.com/terastal-hq/proxylib/clicommand"
)

func runLoginCommand(args []string, cfg config.Config, path string, output io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(output)
	account := fs.String("account", "", "Target account ID")
	replace := fs.Bool("replace", false, "Replace existing account even if Google identity mismatches")
	issueKey := fs.String("issue-key", "", "Mint a new API key with this label upon successful login")

	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := oauth.SessionOptions{
		AccountID: strings.TrimSpace(*account),
		Replace:   *replace,
		IssueKey:  strings.TrimSpace(*issueKey),
	}
	return oauth.LoginWithOptions(cfg, path, opts)
}

func accountCliConfig(cfg config.Config, path string, output io.Writer) clicommand.Config[config.Account] {
	return clicommand.Config[config.Account]{
		AppName: "antigravity-proxy",
		Output:  output,
		GetAccounts: func() ([]config.Account, error) {
			return cfg.Accounts, nil
		},
		UpdateAccounts: func(edit func(*[]config.Account) error) error {
			return config.UpdateAccounts(path, edit)
		},
		GetID:         func(a config.Account) string { return a.ID },
		GetEmail:      func(a config.Account) string { return a.AccountEmail },
		IsEnabled:     func(a config.Account) bool { return a.Enabled },
		SetEnabled:    func(a *config.Account, e bool) { a.Enabled = e },
		GetKeys:       func(a config.Account) []accounts.Key { return a.Keys },
		AddKey:        func(a *config.Account, k accounts.Key) { a.AddKey(k) },
		RemoveKey:     func(a *config.Account, id string) bool { return a.RemoveKey(id) },
		KeyPrefix:     apikey.Prefix,
		GenerateKey:   apikey.Generate,
		GenerateKeyID: apikey.GenerateID,
	}
}

func runAccountsCommand(args []string, cfg config.Config, path string, output io.Writer) error {
	return clicommand.RunAccounts(args, accountCliConfig(cfg, path, output))
}

func runKeysCommand(args []string, cfg config.Config, path string, output io.Writer) error {
	return clicommand.RunKeys(args, accountCliConfig(cfg, path, output))
}

func runAdminKeyCommand(args []string, path string, output io.Writer) error {
	return clicommand.RunAdminKey(args, "antigravity-proxy", output, func(hash string) error {
		if err := config.UpdateAdminKey(path, hash); err != nil {
			return fmt.Errorf("save admin key: %w", err)
		}
		return nil
	}, apikey.Generate)
}
