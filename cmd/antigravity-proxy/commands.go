package main

import (
	"antigravity-proxy/internal/apikey"
	"antigravity-proxy/internal/config"
	"antigravity-proxy/internal/oauth"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

func printLine(w io.Writer, a ...any)                  { _, _ = fmt.Fprintln(w, a...) }
func printFormat(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

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

func runAccountsCommand(args []string, cfg config.Config, path string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: antigravity-proxy accounts <list|enable|disable|remove> [ID]")
	}

	switch args[0] {
	case "list":
		if len(cfg.Accounts) == 0 {
			printLine(output, "No accounts configured.")
			return nil
		}
		printLine(output, "CONFIGURED ACCOUNTS:")
		for _, acc := range cfg.Accounts {
			statusStr := "enabled"
			if !acc.Enabled {
				statusStr = "disabled"
			}
			email := acc.AccountEmail
			if email == "" {
				email = "(not identified)"
			}
			printFormat(output, "  • %-16s [%s] email: %-24s keys: %d\n", acc.ID, statusStr, email, len(acc.Keys))
		}
		return nil

	case "enable", "disable":
		if len(args) < 2 {
			return fmt.Errorf("usage: antigravity-proxy accounts %s <account-id>", args[0])
		}
		targetID := strings.TrimSpace(args[1])
		enableVal := args[0] == "enable"

		found := false
		err := config.UpdateAccounts(path, func(accounts *[]config.Account) error {
			for i := range *accounts {
				if (*accounts)[i].ID == targetID {
					found = true
					(*accounts)[i].Enabled = enableVal
					return nil
				}
			}
			return fmt.Errorf("account %q not found", targetID)
		})
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("account %q not found", targetID)
		}
		printFormat(output, "Account %q %sd.\n", targetID, args[0])
		printLine(output, "Note: If the proxy server is running, restart it to load configuration changes.")
		return nil

	case "remove":
		if len(args) < 2 {
			return errors.New("usage: antigravity-proxy accounts remove <account-id>")
		}
		targetID := strings.TrimSpace(args[1])
		found := false
		err := config.UpdateAccounts(path, func(accounts *[]config.Account) error {
			filtered := make([]config.Account, 0, len(*accounts))
			for _, acc := range *accounts {
				if acc.ID == targetID {
					found = true
					continue
				}
				filtered = append(filtered, acc)
			}
			if !found {
				return fmt.Errorf("account %q not found", targetID)
			}
			*accounts = filtered
			return nil
		})
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("account %q not found", targetID)
		}
		printFormat(output, "Account %q removed.\n", targetID)
		printLine(output, "Note: If the proxy server is running, restart it to load configuration changes.")
		return nil

	default:
		return fmt.Errorf("unknown accounts command %q (valid: list, enable, disable, remove)", args[0])
	}
}

func runKeysCommand(args []string, cfg config.Config, path string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: antigravity-proxy keys <list|create|revoke> <ACCOUNT_ID> [KEY_ID] [--label LABEL]")
	}

	switch args[0] {
	case "list":
		if len(args) < 2 {
			return errors.New("usage: antigravity-proxy keys list <account-id>")
		}
		accountID := strings.TrimSpace(args[1])
		var target *config.Account
		for i := range cfg.Accounts {
			if cfg.Accounts[i].ID == accountID {
				target = &cfg.Accounts[i]
				break
			}
		}
		if target == nil {
			return fmt.Errorf("account %q not found", accountID)
		}
		if len(target.Keys) == 0 {
			printFormat(output, "No API keys configured for account %q.\n", accountID)
			return nil
		}
		printFormat(output, "API KEYS FOR ACCOUNT %q:\n", accountID)
		printFormat(output, "  %-16s %-20s %-8s %s\n", "KEY ID", "LABEL", "LAST4", "CREATED (UTC)")
		for _, k := range target.Keys {
			lbl := k.Label
			if lbl == "" {
				lbl = "-"
			}
			printFormat(output, "  %-16s %-20s %-8s %s\n", k.ID, lbl, k.Last4, k.CreatedAt.Format(time.RFC3339))
		}
		return nil

	case "create":
		if len(args) < 2 {
			return errors.New("usage: antigravity-proxy keys create <account-id> [--label LABEL]")
		}
		accountID := strings.TrimSpace(args[1])
		fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
		fs.SetOutput(output)
		label := fs.String("label", "", "Label for the API key")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}

		rawSecret, err := apikey.Generate()
		if err != nil {
			return fmt.Errorf("generate key: %w", err)
		}
		keyID, err := apikey.GenerateID()
		if err != nil {
			return fmt.Errorf("generate key id: %w", err)
		}

		keyObj := config.AccountKey{
			ID:        keyID,
			Label:     strings.TrimSpace(*label),
			Hash:      apikey.Hash(rawSecret),
			Last4:     apikey.Last4(rawSecret),
			CreatedAt: time.Now().UTC(),
		}

		found := false
		err = config.UpdateAccounts(path, func(accounts *[]config.Account) error {
			for i := range *accounts {
				if (*accounts)[i].ID == accountID {
					found = true
					(*accounts)[i].Keys = append((*accounts)[i].Keys, keyObj)
					return nil
				}
			}
			return fmt.Errorf("account %q not found", accountID)
		})
		if !found {
			return fmt.Errorf("account %q not found", accountID)
		}
		if err != nil {
			return err
		}

		printFormat(output, "Created API key for account %q:\n", accountID)
		printFormat(output, "  Key:   %s\n", rawSecret)
		printFormat(output, "  ID:    %s\n", keyID)
		if keyObj.Label != "" {
			printFormat(output, "  Label: %s\n", keyObj.Label)
		}
		printLine(output, "\nSave this secret key now - it cannot be displayed again.")
		printLine(output, "Note: If the proxy server is running, restart it to load configuration changes.")
		return nil

	case "revoke":
		if len(args) < 3 {
			return errors.New("usage: antigravity-proxy keys revoke <account-id> <key-id>")
		}
		accountID := strings.TrimSpace(args[1])
		keyID := strings.TrimSpace(args[2])

		foundAccount := false
		foundKey := false
		err := config.UpdateAccounts(path, func(accounts *[]config.Account) error {
			for i := range *accounts {
				if (*accounts)[i].ID == accountID {
					foundAccount = true
					filtered := make([]config.AccountKey, 0, len((*accounts)[i].Keys))
					for _, k := range (*accounts)[i].Keys {
						if k.ID == keyID {
							foundKey = true
							continue
						}
						filtered = append(filtered, k)
					}
					if !foundKey {
						return fmt.Errorf("key %q not found", keyID)
					}
					(*accounts)[i].Keys = filtered
					return nil
				}
			}
			return fmt.Errorf("account %q not found", accountID)
		})
		if err != nil {
			return err
		}
		if !foundAccount {
			return fmt.Errorf("account %q not found", accountID)
		}
		if !foundKey {
			return fmt.Errorf("key %q not found in account %q", keyID, accountID)
		}

		printFormat(output, "Key %q revoked from account %q.\n", keyID, accountID)
		printLine(output, "Note: If the proxy server is running, restart it to load configuration changes.")
		return nil

	default:
		return fmt.Errorf("unknown keys command %q (valid: list, create, revoke)", args[0])
	}
}

func runAdminKeyCommand(args []string, path string, output io.Writer) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New("usage: antigravity-proxy admin-key rotate")
	}

	secret, err := apikey.Generate()
	if err != nil {
		return fmt.Errorf("generate admin key: %w", err)
	}

	hash := apikey.Hash(secret)
	if err := config.UpdateAdminKey(path, hash); err != nil {
		return fmt.Errorf("save admin key: %w", err)
	}

	printLine(output, "Rotated admin API key:")
	printFormat(output, "  Key: %s\n\n", secret)
	printLine(output, "Save this key now - it cannot be displayed again.")
	printLine(output, "Note: If the proxy server is running, restart it to apply the new admin key.")
	return nil
}
