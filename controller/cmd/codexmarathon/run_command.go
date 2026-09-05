package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"codexmarathon/controller/app"
	"codexmarathon/controller/internal/automation"
	installedruntime "codexmarathon/controller/internal/runtime"
)

// runCommand is the one-command operational entry point. The default path is
// the user's installed `codex` CLI. The legacy bundled-runtime path remains
// available only when an explicit runtime endpoint/command is supplied, so a
// normal companion install can never accidentally start a second Codex.
func runCommand(args []string, stdout, stderr io.Writer) int {
	if hasRunFlag(args, "--runtime-command") || hasRunFlag(args, "--runtime-endpoint") || hasRunFlag(args, "--runtime") || hasRunFlag(args, "--runtime-arg") {
		return runManagedRuntimeCommand(args, stdout, stderr)
	}
	return runInstalledCompanionCommand(args, stdout, stderr)
}

// runManagedRuntimeCommand is the explicit opt-in path for the imported
// Marathon protocol/runtime fixture. It is intentionally not the product
// default and is kept separate from installed-Codex argument handling.
func runManagedRuntimeCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	endpoint := fs.String("runtime-endpoint", "", "managed local runtime endpoint (default: user-scoped Unix socket or Windows named pipe)")
	fs.StringVar(endpoint, "runtime", "", "alias for --runtime-endpoint")
	runtimeCommand := fs.String("runtime-command", "", "bundled runtime executable override")
	runtimeDir := fs.String("runtime-dir", "", "working directory for the bundled runtime")
	diagnostics := fs.String("diagnostics", "", "redacted lifecycle diagnostics path")
	healthInterval := fs.Duration("health-interval", 5*time.Second, "runtime health-check interval")
	connectTimeout := fs.Duration("connect-timeout", 15*time.Second, "runtime readiness timeout")
	reconnectInterval := fs.Duration("reconnect-interval", 750*time.Millisecond, "initial reconnect delay")
	reconnectMax := fs.Duration("reconnect-max", 8*time.Second, "maximum reconnect delay/attempt window")
	shutdownTimeout := fs.Duration("shutdown-timeout", 10*time.Second, "graceful runtime shutdown timeout")
	noAutoRestart := fs.Bool("no-auto-restart", false, "leave the command stopped after an unexpected runtime exit")
	var runtimeArgs stringListFlag
	fs.Var(&runtimeArgs, "runtime-arg", "argument passed to the bundled runtime (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "run does not accept positional arguments; use --runtime-arg for bundled runtime arguments")
	}
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	config := app.SupervisorConfig{
		Endpoint:           *endpoint,
		RuntimeArgs:        append([]string(nil), runtimeArgs...),
		RuntimeDir:         *runtimeDir,
		HealthInterval:     *healthInterval,
		ConnectTimeout:     *connectTimeout,
		ReconnectInterval:  *reconnectInterval,
		ReconnectMax:       *reconnectMax,
		ShutdownTimeout:    *shutdownTimeout,
		DiagnosticsPath:    *diagnostics,
		DisableAutoRestart: *noAutoRestart,
	}
	if strings.TrimSpace(*runtimeCommand) != "" {
		config.RuntimeCommand = []string{*runtimeCommand}
	}
	supervisor, err := app.NewRuntimeSupervisor(controller, config)
	if err != nil {
		return printError(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := supervisor.Start(ctx); err != nil {
		return printError(stderr, err)
	}
	status := supervisor.Status()
	_, _ = fmt.Fprintf(stdout, "codexmarathon runtime ready: endpoint=%s protocol=%d pid=%d\n", status.Endpoint, status.ProtocolVersion, status.PID)
	monitorDone := make(chan struct{})
	go func() {
		_ = supervisor.Wait()
		close(monitorDone)
	}()
	select {
	case <-ctx.Done():
		if err := supervisor.Stop(); err != nil {
			return printError(stderr, err)
		}
		return 0
	case <-monitorDone:
		status := supervisor.Status()
		if status.State == app.LifecycleFailed {
			return printError(stderr, fmt.Errorf("runtime supervisor stopped: %s", status.LastError))
		}
	}
	return 0
}

// runInstalledCompanionCommand discovers and attaches to the existing Codex
// app-server when it is already running. If no control socket is available it
// launches the same installed executable under CompanionSupervisor; that
// establishes ownership for the controlled restart/resume fallback.
func runInstalledCompanionCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var paths pathFlags
	paths.bind(fs)
	codexPath := fs.String("codex", "", "installed Codex executable (default: codex on PATH or CODEX_BIN)")
	codexHome := fs.String("codex-home", "", "Codex home containing the app-server control socket (default: CODEX_HOME or ~/.codex)")
	workingDir := fs.String("codex-dir", "", "working directory for the installed Codex process")
	controlEndpoint := fs.String("app-server-endpoint", "", "explicit installed app-server endpoint (default: Codex home control socket)")
	attachOnly := fs.Bool("attach-only", false, "fail when no running installed app-server can be attached")
	connectTimeout := fs.Duration("connect-timeout", 15*time.Second, "maximum installed app-server discovery/launch wait")
	shutdownTimeout := fs.Duration("shutdown-timeout", 10*time.Second, "graceful installed Codex shutdown timeout")
	var codexArgs stringListFlag
	fs.Var(&codexArgs, "codex-arg", "argument passed to installed Codex (repeatable; positional arguments are also forwarded)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	controller, err := app.New(paths.config())
	if err != nil {
		return printError(stderr, err)
	}
	defer func() { _ = controller.Close() }()
	if err := controller.EnsureLayout(); err != nil {
		return printError(stderr, err)
	}
	installation, err := app.DiscoverInstalledCodex(ctx, installedruntime.DiscoveryOptions{
		Executable: *codexPath,
		CodexHome:  *codexHome,
	})
	if err != nil {
		return printError(stderr, err)
	}

	endpoint := strings.TrimSpace(*controlEndpoint)
	if endpoint == "" {
		endpoint = installation.ControlSocket
	}
	launchConnectTimeout := *connectTimeout
	if launchConnectTimeout <= 0 {
		launchConnectTimeout = 15 * time.Second
	}
	initialAttachTimeout := 500 * time.Millisecond
	if launchConnectTimeout < initialAttachTimeout {
		initialAttachTimeout = launchConnectTimeout
	}
	attachCtx, attachCancel := context.WithTimeout(ctx, initialAttachTimeout)
	client, attachErr := connectInstalledUntil(attachCtx, installation, endpoint)
	attachCancel()
	if attachErr == nil {
		_, _ = fmt.Fprintf(stdout, "codexmarathon attached to installed codex: executable=%s version=%s mode=live-reload endpoint=%s\n", installation.Executable, valueOr(installation.Version, "unknown"), endpoint)
		return runInstalledCompanionSession(ctx, controller, installation, endpoint, client, nil, launchConnectTimeout, stdout, stderr)
	}
	if *attachOnly {
		return printError(stderr, fmt.Errorf("attach to installed Codex app-server at %s: %w", endpoint, attachErr))
	}

	command := []string{installation.Executable}
	command = append(command, codexArgs...)
	command = append(command, fs.Args()...)
	environment := []string{"CODEX_HOME=" + installation.CodexHome}
	supervisor, err := app.NewCompanionSupervisor(app.CompanionConfig{
		Command:         command,
		WorkingDir:      *workingDir,
		Environment:     environment,
		ShutdownTimeout: *shutdownTimeout,
	})
	if err != nil {
		return printError(stderr, err)
	}
	if err := supervisor.Start(); err != nil {
		return printError(stderr, err)
	}
	status := supervisor.Status()
	_, _ = fmt.Fprintf(stdout, "codexmarathon started installed codex: executable=%s version=%s pid=%d mode=restart-resume-until-control-available\n", installation.Executable, valueOr(installation.Version, "unknown"), status.PID)

	// App-server startup is independent of the interactive CLI process. Give
	// it a bounded opportunity to expose the live control socket, but keep the
	// process usable when this Codex version has no local control interface.
	readyCtx, readyCancel := context.WithTimeout(ctx, launchConnectTimeout)
	client, readyErr := connectInstalledUntilProcess(readyCtx, installation, endpoint, supervisor)
	readyCancel()
	if readyErr == nil {
		_, _ = fmt.Fprintf(stdout, "codexmarathon installed app-server ready: version=%s mode=live-reload\n", valueOr(client.Version(), valueOr(installation.Version, "unknown")))
	} else {
		_, _ = fmt.Fprintf(stdout, "codexmarathon installed app-server unavailable: %v; controlled restart/resume fallback remains available\n", readyErr)
	}
	return runInstalledCompanionSession(ctx, controller, installation, endpoint, client, supervisor, launchConnectTimeout, stdout, stderr)
}

// runInstalledCompanionSession owns the user-facing automation boundary for
// an installed Codex process. Notifications feed the policy loop, and its
// transition callback chooses account/read live reload first, then the
// companion-owned restart/resume supervisor when the app-server cannot reload
// credentials in place.
func runInstalledCompanionSession(ctx context.Context, controller *app.Controller, installation installedruntime.Installation, endpoint string, initial *installedruntime.InstalledClient, supervisor *app.CompanionSupervisor, connectTimeout time.Duration, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if controller == nil {
		return printError(stderr, errors.New("controller is unavailable for installed Codex automation"))
	}
	if connectTimeout <= 0 {
		connectTimeout = 15 * time.Second
	}
	holder := &installedSessionState{client: initial}
	defer func() {
		_ = holder.Close()
		if supervisor != nil {
			_ = supervisor.Stop(context.Background())
		}
	}()
	provider := app.NewInstalledQuotaProvider(holder.Client)
	activeID, err := controller.Registry().ActiveID()
	if err != nil {
		return printError(stderr, err)
	}
	if activeID != "" {
		if err := controller.RegisterUsageProvider(activeID, provider); err != nil {
			return printError(stderr, err)
		}
	}

	// A restart briefly transitions Stopping -> Stopped -> Starting. The
	// lifecycle monitor takes this mutex before treating a process exit as the
	// end of the user-facing command, so an intentional fallback restart cannot
	// race the old process's Wait result.
	var transitionMu sync.Mutex
	transition := func(transitionCtx context.Context, accountID string) error {
		transitionMu.Lock()
		defer transitionMu.Unlock()
		threadID := holder.ThreadID()
		resume := app.CompanionResumeTarget{ThreadID: threadID, Last: threadID == ""}
		result, err := controller.TransitionInstalledOrRestart(transitionCtx, holder.Client(), supervisor, accountID, resume)
		if err != nil {
			return err
		}
		if err := controller.RegisterUsageProvider(accountID, provider); err != nil {
			return err
		}
		if cache := controller.Cache(); cache != nil {
			cache.Invalidate(accountID)
		}
		if result.Mode != app.InstalledTransitionRestart {
			return nil
		}

		connectCtx, cancel := context.WithTimeout(transitionCtx, connectTimeout)
		next, connectErr := connectInstalledUntil(connectCtx, installation, endpoint)
		cancel()
		if connectErr != nil {
			return fmt.Errorf("reconnect installed Codex app-server after restart: %w", connectErr)
		}
		// The initialize handshake proves the control endpoint is alive. Read
		// and compare the deployed auth identity before replacing the
		// provider's connection or confirming the policy transition.
		if _, verifyErr := controller.VerifyInstalledIdentity(transitionCtx, next, accountID); verifyErr != nil {
			_ = next.Close()
			return fmt.Errorf("verify resumed installed Codex identity: %w", verifyErr)
		}
		holder.SetClient(next)
		return nil
	}

	loop, err := controller.NewAutomationLoopWithTransition(transition)
	if err != nil {
		return printError(stderr, err)
	}
	events := make(chan automation.Event, 256)
	loopCtx, cancelLoop := context.WithCancel(ctx)
	defer cancelLoop()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		if loopErr := loop.Run(loopCtx, events); loopErr != nil && !errors.Is(loopErr, context.Canceled) {
			_, _ = fmt.Fprintf(stderr, "codexmarathon installed automation stopped: %v\n", loopErr)
		}
	}()

	// Evaluate once at startup so a configured active account gets a fresh
	// snapshot and a first-run installation can select an eligible account.
	events <- automation.Event{Type: automation.EventTelemetryChanged, OccurredAt: time.Now().UTC()}
	go pumpInstalledNotifications(loopCtx, holder, controller, events)

	// Keep non-fatal provider/policy errors visible while preserving Codex's
	// interactive stdout/stderr streams. Errors are already redacted by the
	// controller's event boundary.
	errorDone := make(chan struct{})
	go func() {
		defer close(errorDone)
		diagnostics := controller.EventErrors()
		for {
			select {
			case err := <-diagnostics:
				if err != nil {
					_, _ = fmt.Fprintf(stderr, "codexmarathon installed automation: %v\n", err)
				}
			case <-loopCtx.Done():
				return
			}
		}
	}()

	var waitDone chan error
	if supervisor != nil {
		waitDone = make(chan error, 1)
		go func() { waitDone <- supervisor.Wait(context.Background()) }()
	}
	for {
		select {
		case <-ctx.Done():
			cancelLoop()
			_ = holder.Close()
			if supervisor != nil {
				transitionMu.Lock()
				stopErr := supervisor.Stop(context.Background())
				transitionMu.Unlock()
				if stopErr != nil {
					return printError(stderr, stopErr)
				}
			}
			<-loopDone
			<-errorDone
			return 0
		case waitErr := <-waitDone:
			// A controlled restart consumes the old Wait result. Once the
			// transition callback releases the mutex, inspect the new state
			// before deciding whether the command really ended.
			transitionMu.Lock()
			status := supervisor.Status()
			transitionMu.Unlock()
			if status.State == app.CompanionRunning || status.State == app.CompanionStarting {
				waitDone = make(chan error, 1)
				go func() { waitDone <- supervisor.Wait(context.Background()) }()
				continue
			}
			cancelLoop()
			_ = holder.Close()
			<-loopDone
			<-errorDone
			if waitErr != nil {
				return printError(stderr, fmt.Errorf("installed Codex stopped: %w", waitErr))
			}
			return 0
		}
	}
}

type installedSessionState struct {
	mu       sync.RWMutex
	client   *installedruntime.InstalledClient
	threadID string
}

func (s *installedSessionState) Client() *installedruntime.InstalledClient {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	return client
}

func (s *installedSessionState) SetClient(client *installedruntime.InstalledClient) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

func (s *installedSessionState) ClearClient(client *installedruntime.InstalledClient) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.client == client {
		s.client = nil
	}
	s.mu.Unlock()
}

func (s *installedSessionState) ThreadID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	threadID := s.threadID
	s.mu.RUnlock()
	return threadID
}

func (s *installedSessionState) SetThreadID(threadID string) {
	if s == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	s.mu.Lock()
	s.threadID = strings.TrimSpace(threadID)
	s.mu.Unlock()
}

func (s *installedSessionState) Close() error {
	if s == nil {
		return nil
	}
	client := s.Client()
	if client == nil {
		return nil
	}
	return client.Close()
}

func pumpInstalledNotifications(ctx context.Context, holder *installedSessionState, controller *app.Controller, events chan<- automation.Event) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		client := holder.Client()
		if client == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		notifications := client.Notifications()
		if notifications == nil {
			holder.ClearClient(client)
			continue
		}
	notificationLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case notification, ok := <-notifications:
				if !ok {
					holder.ClearClient(client)
					continue notificationLoop
				}
				accountID := ""
				if controller != nil && controller.Registry() != nil {
					accountID, _ = controller.Registry().ActiveID()
				}
				if notification.Method == "account/rateLimits/updated" && accountID != "" && controller != nil {
					// Preserve the event even when a future server sends an
					// ambiguous sparse shape; the automation loop will report the
					// provider failure instead of authorizing a stale switch.
					_ = controller.IngestInstalledRateLimitsUpdate(ctx, accountID, notification.Params)
				}
				event, threadID, emit := app.InstalledNotificationAutomationEvent(notification, accountID, time.Now().UTC())
				if threadID != "" {
					holder.SetThreadID(threadID)
				}
				if !emit {
					continue
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func connectInstalledUntil(ctx context.Context, installation installedruntime.Installation, endpoint string) (*installedruntime.InstalledClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = installation.ControlSocket
	}
	var lastErr error
	delay := 100 * time.Millisecond
	for {
		client, err := app.ConnectInstalledCodexEndpoint(ctx, endpoint)
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("%w: %v", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

// connectInstalledUntilProcess is the launch variant of the bounded connect
// loop. A CLI that exits before exposing a control socket should be reported
// immediately rather than making the companion wait for the entire readiness
// timeout. It only reads supervisor status; process ownership remains with the
// session lifecycle below.
func connectInstalledUntilProcess(ctx context.Context, installation installedruntime.Installation, endpoint string, supervisor *app.CompanionSupervisor) (*installedruntime.InstalledClient, error) {
	if supervisor == nil {
		return connectInstalledUntil(ctx, installation, endpoint)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	delay := 100 * time.Millisecond
	for {
		status := supervisor.Status()
		if status.State == app.CompanionStopped || status.State == app.CompanionFailed {
			if lastErr == nil {
				lastErr = errors.New("installed Codex exited before its app-server became available")
			}
			return nil, fmt.Errorf("%w: %v", installedruntime.ErrAppServerUnavailable, lastErr)
		}
		client, err := app.ConnectInstalledCodexEndpoint(ctx, endpoint)
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("%w: %v", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func hasRunFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, " ")
}

func (f *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("runtime argument must not be empty")
	}
	*f = append(*f, value)
	return nil
}
