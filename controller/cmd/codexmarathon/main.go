// Command codexmarathon is the decision-first operator CLI and one-command
// launcher for the user's installed Codex CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"codexmarathon/controller/app"
	"codexmarathon/controller/internal/journal"
	"codexmarathon/controller/internal/policy"
	installedruntime "codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/transitions"
)

const commandUsage = `CodexMarathon controller

Usage:
  codexmarathon run [options]
  codexmarathon start [options]
  codexmarathon init [options]
  codexmarathon status [options]
  codexmarathon accounts list [options]
  codexmarathon accounts login [options]
  codexmarathon accounts add [options]
  codexmarathon accounts import [options]
  codexmarathon accounts status [account-id] [options]
  codexmarathon accounts rename --name <alias> <account-id> [options]
  codexmarathon accounts activate <account-id> [options]
  codexmarathon accounts use <account-id> [options]
  codexmarathon accounts refresh <account-id> --runtime <endpoint> [options]
  codexmarathon accounts remove <account-id> [options]
  codexmarathon transition <account-id> [options]
  codexmarathon switch <account-id> [options]
  codexmarathon reconcile [transition-id] --runtime <endpoint> [options]

Options are command-local. The launcher discovers the installed Codex CLI from
--codex, CODEX_BIN, or PATH. It prefers the installed app-server's user-scoped
local control socket and uses controlled restart/resume when live reload is not
available. A bundled Marathon runtime is used only with explicit runtime flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		_, _ = io.WriteString(stdout, commandUsage)
		return 0
	}
	switch args[0] {
	case "run", "start":
		return runCommand(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "accounts":
		return runAccounts(args[1:], stdout, stderr)
	case "transition":
		return runTransition(args[1:], stdout, stderr)
	case "switch":
		return runTransition(args[1:], stdout, stderr)
	case "reconcile":
		return runReconcile(args[1:], stdout, stderr)
	default:
		return usageError(stderr, fmt.Sprintf("unknown command %q", args[0]))
	}
}

type pathFlags struct {
	stateDir     string
	registryPath string
	vaultDir     string
	authPath     string
	journalPath  string
}

func (p *pathFlags) bind(fs *flag.FlagSet) {
	defaults := app.DefaultConfig()
	fs.StringVar(&p.stateDir, "state-dir", defaults.StateDir, "controller state directory")
	fs.StringVar(&p.registryPath, "registry", "", "account registry path (default: state-dir/accounts.json)")
	fs.StringVar(&p.vaultDir, "vault", "", "credential vault directory (default: state-dir/credentials)")
	fs.StringVar(&p.authPath, "auth", defaults.AuthPath, "Codex auth.json path used for deployment/reconciliation")
	fs.StringVar(&p.journalPath, "journal", "", "journal path (default: state-dir/events.jsonl)")
}

func (p pathFlags) config() app.Config {
	return app.Config{
		StateDir:     p.stateDir,
		RegistryPath: p.registryPath,
		VaultDir:     p.vaultDir,
		AuthPath:     p.authPath,
		JournalPath:  p.journalPath,
	}
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "init does not accept positional arguments")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	if err := controller.EnsureLayout(); err != nil {
		return printError(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "initialized controller state at %s\n", controller.Config().StateDir)
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	runtimeEndpoint := fs.String("runtime", "", "optional runtime endpoint to query")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "status does not accept positional arguments")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	if *runtimeEndpoint != "" {
		ctx, cancel := context.WithTimeout(context.Background(), app.DefaultConnectTimeout)
		err = controller.ConnectRuntimeEndpoint(ctx, *runtimeEndpoint)
		cancel()
		if err != nil {
			return printError(stderr, err)
		}
	}
	accounts, err := controller.Registry().List()
	if err != nil {
		return printError(stderr, err)
	}
	telemetry, err := controller.AccountsTelemetry()
	if err != nil {
		return printError(stderr, err)
	}
	decision, err := controller.EvaluatePolicy(time.Now().UTC())
	if err != nil {
		return printError(stderr, err)
	}
	active, activeSet, err := controller.Registry().Active()
	if err != nil {
		return printError(stderr, err)
	}
	status := statusView{
		StateDir:          controller.Config().StateDir,
		RegistryPath:      controller.Config().RegistryPath,
		VaultDir:          controller.Config().VaultDir,
		AuthPath:          controller.Config().AuthPath,
		JournalPath:       controller.Config().JournalPath,
		AccountCount:      len(accounts),
		TelemetryAccounts: len(telemetry),
		RuntimeConnected:  controller.RuntimeConnected(),
		Policy:            makeDecisionView(decision),
	}
	if activeSet {
		status.ActiveAccountID = active.ID
	}
	if controller.RuntimeConnected() {
		state, stateErr := controller.RuntimeState(context.Background())
		if stateErr == nil {
			status.Runtime = &runtimeView{
				RuntimeID:         state.Identity.RuntimeID,
				AccountID:         optionalID(state.Identity.AccountID),
				AuthGeneration:    state.Identity.AuthGeneration,
				ActiveTurnCount:   state.ActiveTurnCount,
				PendingTransition: state.PendingTransition != nil,
			}
		} else {
			status.RuntimeError = stateErr.Error()
		}
	}
	if *jsonOutput {
		return writeJSON(stdout, status)
	}
	_, _ = fmt.Fprintf(stdout, "state: %s\naccounts: %d\nactive: %s\ntelemetry observations: %d\npolicy: %s\n", status.StateDir, status.AccountCount, valueOr(status.ActiveAccountID, "none"), status.TelemetryAccounts, status.Policy.Type)
	if status.Runtime != nil {
		_, _ = fmt.Fprintf(stdout, "runtime: %s account=%s generation=%d turns=%d pending=%t\n", status.Runtime.RuntimeID, valueOr(status.Runtime.AccountID, "none"), status.Runtime.AuthGeneration, status.Runtime.ActiveTurnCount, status.Runtime.PendingTransition)
	} else if status.RuntimeError != "" {
		_, _ = fmt.Fprintf(stdout, "runtime: unavailable (%s)\n", status.RuntimeError)
	} else {
		_, _ = fmt.Fprintln(stdout, "runtime: not connected")
	}
	if status.Policy.Reason != "" {
		_, _ = fmt.Fprintf(stdout, "policy reason: %s\n", status.Policy.Reason)
	}
	return 0
}

func runAccountsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accounts list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "accounts list does not accept positional arguments")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	accounts, err := controller.Registry().List()
	if err != nil {
		return printError(stderr, err)
	}
	if *jsonOutput {
		return writeJSON(stdout, accounts)
	}
	if len(accounts) == 0 {
		_, _ = fmt.Fprintln(stdout, "no accounts configured")
		return 0
	}
	for _, account := range accounts {
		health := string(account.CredentialHealth)
		_, _ = fmt.Fprintf(stdout, "%s\t%s\tcredential=%s\ttelemetry=%s\n", account.ID, valueOr(account.Alias, "-"), valueOr(health, "unknown"), valueOr(account.TelemetrySource, "none"))
	}
	return 0
}

func runTransition(args []string, stdout, stderr io.Writer) int {
	if !hasRunFlag(args, "--runtime") {
		return runInstalledTransition(args, stdout, stderr)
	}
	return runManagedTransition(args, stdout, stderr)
}

// runManagedTransition retains the explicit legacy Marathon protocol path.
// It is selected only when --runtime is supplied; normal transition/switch
// commands use the installed Codex app-server below.
func runManagedTransition(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("transition", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	runtimeEndpoint := fs.String("runtime", "", "runtime endpoint (tcp://host:port or host:port)")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum transition duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "transition requires exactly one target account id")
	}
	if strings.TrimSpace(*runtimeEndpoint) == "" {
		return usageError(stderr, "transition requires --runtime")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := controller.ConnectRuntimeEndpoint(ctx, *runtimeEndpoint); err != nil {
		return printError(stderr, err)
	}
	result, err := controller.RequestTransition(ctx, fs.Arg(0))
	if result != nil && *jsonOutput {
		_ = writeJSON(stdout, result)
	} else if result != nil {
		_, _ = fmt.Fprintf(stdout, "transition %s: %s account=%s generation=%d\n", result.TransitionID, result.Outcome, valueOr(result.AdoptedAccountID, "unknown"), result.FinalGeneration)
	}
	if err != nil {
		return printError(stderr, err)
	}
	return 0
}

func runInstalledTransition(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("transition", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	codexPath := fs.String("codex", "", "installed Codex executable (default: codex on PATH or CODEX_BIN)")
	codexHome := fs.String("codex-home", "", "Codex home containing the app-server control socket")
	controlEndpoint := fs.String("app-server-endpoint", "", "explicit installed app-server endpoint")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum transition duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return usageError(stderr, "transition requires exactly one target account id")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	installation, err := app.DiscoverInstalledCodex(ctx, installedDiscoveryOptions(*codexPath, *codexHome))
	if err != nil {
		return printError(stderr, err)
	}
	endpoint := strings.TrimSpace(*controlEndpoint)
	if endpoint == "" {
		endpoint = installation.ControlSocket
	}
	client, err := app.ConnectInstalledCodexEndpoint(ctx, endpoint)
	if err != nil {
		return printError(stderr, fmt.Errorf("connect installed Codex app-server at %s: %w; start Codex through `codexmarathon run` to enable controlled restart/resume when live reload is unavailable", endpoint, err))
	}
	defer func() { _ = client.Close() }()
	result, err := controller.TransitionInstalled(ctx, client, fs.Arg(0))
	if result.AccountID != "" && *jsonOutput {
		_ = writeJSON(stdout, result)
	} else if result.AccountID != "" {
		_, _ = fmt.Fprintf(stdout, "transition: mode=%s account=%s verified=%t\n", result.Mode, result.AccountID, result.IdentityVerified)
	}
	if err != nil {
		return printError(stderr, err)
	}
	return 0
}

func installedDiscoveryOptions(codexPath, codexHome string) installedruntime.DiscoveryOptions {
	return installedruntime.DiscoveryOptions{Executable: codexPath, CodexHome: codexHome}
}

func runReconcile(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	runtimeEndpoint := fs.String("runtime", "", "runtime endpoint (tcp://host:port or host:port)")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum reconciliation duration")
	jsonOutput := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		return usageError(stderr, "reconcile accepts at most one transition id")
	}
	if strings.TrimSpace(*runtimeEndpoint) == "" {
		return usageError(stderr, "reconcile requires --runtime")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := controller.ConnectRuntimeEndpoint(ctx, *runtimeEndpoint); err != nil {
		return printError(stderr, err)
	}
	transitionID := ""
	if fs.NArg() == 1 {
		transitionID = fs.Arg(0)
	}
	result, err := controller.Reconcile(ctx, transitionID)
	if result != nil && *jsonOutput {
		_ = writeJSON(stdout, result)
	} else if result != nil {
		_, _ = fmt.Fprintf(stdout, "reconciliation %s: %s account=%s generation=%d\n", result.TransitionID, result.Outcome, valueOr(result.AdoptedAccountID, "unknown"), result.FinalGeneration)
	}
	if err != nil {
		return printError(stderr, err)
	}
	return 0
}

type statusView struct {
	StateDir          string       `json:"state_dir"`
	RegistryPath      string       `json:"registry_path"`
	VaultDir          string       `json:"vault_dir"`
	AuthPath          string       `json:"auth_path"`
	JournalPath       string       `json:"journal_path"`
	AccountCount      int          `json:"account_count"`
	ActiveAccountID   string       `json:"active_account_id,omitempty"`
	TelemetryAccounts int          `json:"telemetry_accounts"`
	RuntimeConnected  bool         `json:"runtime_connected"`
	Runtime           *runtimeView `json:"runtime,omitempty"`
	RuntimeError      string       `json:"runtime_error,omitempty"`
	Policy            decisionView `json:"policy"`
}

type runtimeView struct {
	RuntimeID         string `json:"runtime_id"`
	AccountID         string `json:"account_id,omitempty"`
	AuthGeneration    uint64 `json:"auth_generation"`
	ActiveTurnCount   int    `json:"active_turn_count"`
	PendingTransition bool   `json:"pending_transition"`
}

type decisionView struct {
	Type               string `json:"type"`
	AccountID          string `json:"account_id,omitempty"`
	CandidateAccountID string `json:"candidate_account_id,omitempty"`
	LimitID            string `json:"limit_id,omitempty"`
	DurationMins       int    `json:"duration_mins,omitempty"`
	ResetAt            string `json:"reset_at,omitempty"`
	RefetchRequired    bool   `json:"refetch_required"`
	Reason             string `json:"reason,omitempty"`
}

func makeDecisionView(decision policy.PolicyDecision) decisionView {
	view := decisionView{
		Type:               decision.Type.String(),
		AccountID:          decision.AccountID,
		CandidateAccountID: decision.CandidateAccountID,
		LimitID:            decision.LimitID,
		DurationMins:       decision.DurationMins,
		RefetchRequired:    decision.RefetchRequired,
		Reason:             decision.Reason,
	}
	if !decision.ResetAt.IsZero() {
		view.ResetAt = decision.ResetAt.UTC().Format(time.RFC3339)
	}
	return view
}

func optionalID(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func writeJSON(writer io.Writer, value any) int {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return printError(os.Stderr, err)
	}
	return 0
}

func usageError(stderr io.Writer, message string) int {
	_, _ = fmt.Fprintf(stderr, "error: %s\n\n%s", message, commandUsage)
	return 2
}

func printError(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "error: %s\n", err)
	if errors.Is(err, transitions.ErrReconciliationNeeded) {
		_, _ = fmt.Fprintln(stderr, "hint: inspect runtime/disk state and retry `reconcile` after reconnecting the same runtime")
	}
	return 1
}

// Keep the journal package in the command's static dependency graph even when
// a build strips optional status output. This compile-time assertion also
// documents that RuntimeConnected/Disconnected records use the shared type.
var _ journal.EventType = journal.RuntimeConnected
