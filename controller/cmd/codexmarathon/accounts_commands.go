package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"codexmarathon/controller/app"
	"codexmarathon/controller/internal/accounts"
)

func runAccounts(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		_, _ = io.WriteString(stdout, accountsUsage)
		return 0
	}
	switch args[0] {
	case "list":
		return runAccountsList(args[1:], stdout, stderr)
	case "login", "add":
		return runAccountsLogin(args[1:], stdout, stderr)
	case "status":
		return runAccountsStatus(args[1:], stdout, stderr)
	case "rename":
		return runAccountsRename(args[1:], stdout, stderr)
	case "activate", "use":
		return runAccountsActivate(args[1:], stdout, stderr)
	case "refresh":
		return runAccountsRefresh(args[1:], stdout, stderr)
	case "remove":
		return runAccountsRemove(args[1:], stdout, stderr)
	default:
		return usageError(stderr, fmt.Sprintf("unknown accounts command %q", args[0]))
	}
}

const accountsUsage = `CodexMarathon account manager

Usage:
  codexmarathon accounts list [options]
  codexmarathon accounts login [options]
  codexmarathon accounts add [options]
  codexmarathon accounts status [account-id] [options]
  codexmarathon accounts rename --name <alias> <account-id> [options]
  codexmarathon accounts activate <account-id> [options]
  codexmarathon accounts use <account-id> [options]
  codexmarathon accounts refresh <account-id> --runtime <endpoint> [options]
  codexmarathon accounts remove <account-id> [options]

`

func runAccountsLogin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	accountID := fs.String("id", "", "optional stable account ID (native login identity is used by default)")
	alias := fs.String("name", "", "local profile alias")
	activate := fs.Bool("activate", false, "make this profile active after login")
	overwrite := fs.Bool("overwrite", false, "replace an existing profile with the same account ID")
	runtimeEndpoint := fs.String("runtime", "", "embedded runtime endpoint for native login")
	timeout := fs.Duration("timeout", 10*time.Minute, "maximum login duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "accounts login does not accept positional arguments; use --id and --name")
	}
	if strings.TrimSpace(*runtimeEndpoint) == "" {
		return usageError(stderr, "accounts login requires --runtime so the integrated Codex login service can be used")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := attachAuthRuntime(ctx, controller, *runtimeEndpoint); err != nil {
		return printError(stderr, err)
	}
	outcome, err := controller.AccountManager().Login(ctx, accounts.LoginRequest{
		AccountID: *accountID,
		Alias:     *alias,
		Activate:  *activate,
		Overwrite: *overwrite,
	})
	if err != nil {
		return printError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, outcome)
	}
	if outcome.Activated {
		_, _ = fmt.Fprintf(stdout, "saved and activated account: %s\n", outcome.AccountID)
	} else {
		_, _ = fmt.Fprintf(stdout, "saved account: %s\n", outcome.AccountID)
	}
	return 0
}

func runAccountsStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		return usageError(stderr, "accounts status accepts at most one account ID")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	if fs.NArg() == 1 {
		status, err := controller.AccountManager().Status(fs.Arg(0))
		if err != nil {
			return printError(stderr, err)
		}
		if *jsonOutput {
			return writeJSON(stdout, status)
		}
		printProfileStatus(stdout, status)
		return 0
	}
	statuses, err := controller.AccountManager().List()
	if err != nil {
		return printError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, statuses)
	}
	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(stdout, "no accounts configured")
		return 0
	}
	for _, status := range statuses {
		printProfileStatus(stdout, status)
	}
	return 0
}

func runAccountsRename(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts rename", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	alias := fs.String("name", "", "new local profile alias")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "accounts rename requires exactly one account ID")
	}
	if strings.TrimSpace(*alias) == "" {
		return usageError(stderr, "accounts rename requires --name <alias>")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	if err := controller.AccountManager().Rename(fs.Arg(0), *alias); err != nil {
		return printError(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "renamed account: %s\n", fs.Arg(0))
	return 0
}

func runAccountsActivate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts activate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "accounts activate requires exactly one account ID")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	outcome, err := controller.AccountManager().Activate(context.Background(), fs.Arg(0))
	if err != nil {
		return printError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, outcome)
	}
	_, _ = fmt.Fprintf(stdout, "active account: %s\n", outcome.AccountID)
	return 0
}

func runAccountsRefresh(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts refresh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	runtimeEndpoint := fs.String("runtime", "", "embedded runtime endpoint for native refresh")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum refresh duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "accounts refresh requires exactly one account ID")
	}
	if strings.TrimSpace(*runtimeEndpoint) == "" {
		return usageError(stderr, "accounts refresh requires --runtime so the integrated Codex AuthManager can be used")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := attachAuthRuntime(ctx, controller, *runtimeEndpoint); err != nil {
		return printError(stderr, err)
	}
	outcome, err := controller.AccountManager().Refresh(ctx, fs.Arg(0))
	if err != nil {
		return printError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, outcome)
	}
	_, _ = fmt.Fprintf(stdout, "refreshed account: %s at %s\n", outcome.AccountID, outcome.RefreshedAt.UTC().Format(time.RFC3339))
	return 0
}

func runAccountsRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	force := fs.Bool("force", false, "remove an active profile and clear its controller marker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "accounts remove requires exactly one account ID")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	if err := controller.AccountManager().Remove(fs.Arg(0), *force); err != nil {
		return printError(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "removed account: %s\n", fs.Arg(0))
	return 0
}

func attachAuthRuntime(ctx context.Context, controller *app.Controller, endpoint string) error {
	if controller == nil {
		return errors.New("controller is nil")
	}
	if err := controller.ConnectRuntimeEndpoint(ctx, endpoint); err != nil {
		return err
	}
	controller.SetAuthService(app.NewRuntimeAuthService(controller.RuntimeClient()))
	return nil
}

func printProfileStatus(out io.Writer, status accounts.ProfileStatus) {
	active := ""
	if status.Active {
		active = " active"
	}
	credential := "missing"
	if status.CredentialPresent {
		credential = "present"
	}
	_, _ = fmt.Fprintf(out, "%s\t%s\tcredential=%s health=%s%s", status.AccountID, valueOr(status.Alias, "-"), credential, valueOr(string(status.CredentialHealth), "unknown"), active)
	if status.SavedAt != nil {
		_, _ = fmt.Fprintf(out, " saved=%s", status.SavedAt.UTC().Format(time.RFC3339))
	}
	if status.TelemetrySource != "" {
		_, _ = fmt.Fprintf(out, " telemetry=%s", status.TelemetrySource)
	}
	_, _ = fmt.Fprintln(out)
}
