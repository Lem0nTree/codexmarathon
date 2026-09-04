package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"codexmarathon/controller/app"
)

// runCommand is the one-command operational entry point.  It starts the
// bundled CodexMarathon runtime, waits for the authenticated local IPC
// handshake, and leaves the supervisor running until the user asks it to
// stop.  Runtime command/args are argv values, never a shell expression.
func runCommand(args []string, stdout, stderr io.Writer) int {
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
