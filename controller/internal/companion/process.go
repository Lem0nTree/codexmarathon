// Package companion owns the process boundary used when CodexMarathon is
// installed beside an existing Codex CLI.
//
// A credential transition cannot safely replace auth.json while Codex is
// running: the CLI may have the old credentials cached in memory and can
// write them back while shutting down. RestartAndResume therefore enforces a
// strict order:
//
//  1. ask the launched Codex process to exit gracefully;
//  2. wait until it has really exited (escalating only after the deadline);
//  3. run the caller's credential deployment hook;
//  4. launch the installed Codex executable with `resume` and the same
//     conversation target.
//
// The package intentionally does not read, parse, or log auth material. The
// caller owns the atomic credential deployment and supplies only a callback
// for that step.
package companion

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrNilSupervisor       = errors.New("nil Codex companion supervisor")
	ErrAlreadyStarted      = errors.New("Codex companion is already started")
	ErrNotStarted          = errors.New("Codex companion is not started")
	ErrResumeTargetMissing = errors.New("a conversation id or --last is required to resume Codex")
	ErrResumeTargetInvalid = errors.New("conversation id and --last cannot be used together")
)

const (
	defaultShutdownTimeout = 10 * time.Second
	defaultKillTimeout     = 5 * time.Second
)

// ResumeTarget identifies the conversation Codex should reopen after a
// controlled restart. Exactly one of ThreadID and Last must be set.
// ExtraArgs are appended after the resume target and are useful for stable
// Codex CLI options such as --no-alt-screen. They are argv values, never a
// shell expression.
type ResumeTarget struct {
	ThreadID   string
	Last       bool
	WorkingDir string
	ExtraArgs  []string
}

func (t ResumeTarget) validate() error {
	threadID := strings.TrimSpace(t.ThreadID)
	if threadID == "" && !t.Last {
		return ErrResumeTargetMissing
	}
	if threadID != "" && t.Last {
		return ErrResumeTargetInvalid
	}
	for _, arg := range t.ExtraArgs {
		if strings.TrimSpace(arg) == "" {
			return errors.New("resume argument must not be empty")
		}
	}
	if dir := strings.TrimSpace(t.WorkingDir); dir != "" {
		if !filepath.IsAbs(dir) {
			return errors.New("resume working directory must be absolute")
		}
	}
	return nil
}

// BuildResumeCommand returns an argv-safe invocation of the installed Codex
// CLI's native resume command. It performs no process or filesystem side
// effects, making it suitable for callers that need to preview or journal the
// selected conversation target without exposing credentials.
func BuildResumeCommand(command []string, target ResumeTarget) ([]string, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("installed Codex command is required")
	}
	if isBundledRuntimeCommand(command[0]) {
		return nil, errors.New("companion command must be the installed codex CLI, not a bundled app-server runtime")
	}
	if err := target.validate(); err != nil {
		return nil, err
	}
	result := append([]string(nil), command...)
	result = append(result, "resume")
	if threadID := strings.TrimSpace(target.ThreadID); threadID != "" {
		result = append(result, threadID)
	} else {
		result = append(result, "--last")
	}
	if dir := strings.TrimSpace(target.WorkingDir); dir != "" {
		result = append(result, "--cd", dir)
	}
	result = append(result, target.ExtraArgs...)
	return result, nil
}

// Config controls the installed Codex command. Command is argv-shaped and
// must begin with the user's installed `codex` executable. The supervisor
// never searches for or starts a bundled app-server.
type Config struct {
	Command         []string
	WorkingDir      string
	Environment     []string
	ShutdownTimeout time.Duration
	KillTimeout     time.Duration
	ProcessFactory  ProcessFactory
}

func (c Config) withDefaults() (Config, error) {
	if len(c.Command) == 0 || strings.TrimSpace(c.Command[0]) == "" {
		return Config{}, errors.New("installed Codex command is required")
	}
	if isBundledRuntimeCommand(c.Command[0]) {
		return Config{}, errors.New("companion command must be the installed codex CLI, not a bundled app-server runtime")
	}
	if strings.TrimSpace(c.WorkingDir) != "" {
		if !filepath.IsAbs(c.WorkingDir) {
			return Config{}, errors.New("Codex working directory must be absolute")
		}
		info, err := os.Stat(c.WorkingDir)
		if err != nil {
			return Config{}, fmt.Errorf("stat Codex working directory: %w", err)
		}
		if !info.IsDir() {
			return Config{}, errors.New("Codex working directory is not a directory")
		}
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.KillTimeout <= 0 {
		c.KillTimeout = defaultKillTimeout
	}
	if c.ProcessFactory == nil {
		c.ProcessFactory = NewExecProcess
	}
	c.Command = append([]string(nil), c.Command...)
	c.Environment = append([]string(nil), c.Environment...)
	return c, nil
}

// Process is the small lifecycle seam needed by Supervisor. Done is a
// broadcast completion signal; WaitError returns the process wait result once
// Done has closed.
type Process interface {
	PID() int
	// Done is closed after the process has been waited. It is a broadcast
	// signal, so multiple lifecycle callers can observe one exit safely.
	Done() <-chan struct{}
	WaitError() error
	Interrupt() error
	Terminate() error
	Kill() error
}

// ProcessFactory starts one argv-shaped command. It receives a copy of the
// configured environment and must not mutate the caller's slices.
type ProcessFactory func(command []string, workingDir string, environment []string) (Process, error)

// ExecProcess is the production Process implementation backed by os/exec.
type ExecProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitMu  sync.RWMutex
	waitErr error
}

// NewExecProcess starts an installed Codex process. stdout/stderr are
// inherited so Codex remains an interactive CLI when launched through the
// companion. The process is placed in its own group on Unix so shutdown does
// not leave an app-server child behind.
func NewExecProcess(command []string, workingDir string, environment []string) (Process, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("Codex command is empty")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = workingDir
	// Keep the installed Codex CLI interactive when Marathon launches it.
	// os/exec otherwise connects stdin to the null device and discards stdout
	// and stderr when these fields are left nil.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(environment) > 0 {
		cmd.Env = append(os.Environ(), environment...)
	}
	configureProcessCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start installed Codex %q: %w", command[0], err)
	}
	process := &ExecProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (p *ExecProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *ExecProcess) Done() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.done
}

func (p *ExecProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.waitMu.RLock()
	err := p.waitErr
	p.waitMu.RUnlock()
	return err
}

func (p *ExecProcess) Interrupt() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	return interruptProcess(p.cmd)
}

func (p *ExecProcess) Terminate() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	return terminateProcess(p.cmd)
}

func (p *ExecProcess) Kill() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	return killProcess(p.cmd)
}

// State describes the process lifecycle without including environment or
// credentials.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

type Status struct {
	State          State     `json:"state"`
	PID            int       `json:"pid,omitempty"`
	Command        string    `json:"command,omitempty"`
	RestartCount   int       `json:"restart_count"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	StoppedAt      time.Time `json:"stopped_at,omitempty"`
	LastExit       string    `json:"last_exit,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastResumeID   string    `json:"last_resume_id,omitempty"`
	LastResumeLast bool      `json:"last_resume_last,omitempty"`
}

// Supervisor controls one installed Codex process. A supervisor only tracks
// processes it started; it never guesses at or signals an unrelated Codex
// process owned by the user.
type Supervisor struct {
	config Config

	mu        sync.RWMutex
	process   Process
	starting  bool
	startDone chan struct{}
	status    Status
}

func NewSupervisor(config Config) (*Supervisor, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		config: config,
		status: Status{State: StateStopped, Command: config.Command[0]},
	}, nil
}

// Status returns a race-free snapshot.
func (s *Supervisor) Status() Status {
	if s == nil {
		return Status{State: StateFailed, LastError: ErrNilSupervisor.Error()}
	}
	s.mu.RLock()
	status := s.status
	s.mu.RUnlock()
	return status
}

// Start launches the installed Codex command supplied in Config.Command.
func (s *Supervisor) Start() error {
	return s.start(s.config.Command, s.config.WorkingDir, ResumeTarget{})
}

func (s *Supervisor) start(command []string, workingDir string, resume ResumeTarget) error {
	if s == nil {
		return ErrNilSupervisor
	}
	s.mu.Lock()
	if s.process != nil || s.starting {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.status.State = StateStarting
	s.status.LastError = ""
	s.status.Command = command[0]
	s.mu.Unlock()

	process, err := s.config.ProcessFactory(
		append([]string(nil), command...),
		workingDir,
		append([]string(nil), s.config.Environment...),
	)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		close(s.startDone)
		s.startDone = nil
		s.status.State = StateFailed
		s.status.LastError = err.Error()
		s.mu.Unlock()
		return err
	}
	if process == nil {
		err = errors.New("Codex process factory returned nil process")
		s.mu.Lock()
		s.starting = false
		close(s.startDone)
		s.startDone = nil
		s.status.State = StateFailed
		s.status.LastError = err.Error()
		s.mu.Unlock()
		return err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.starting = false
	close(s.startDone)
	s.startDone = nil
	s.process = process
	s.status.State = StateRunning
	s.status.PID = process.PID()
	s.status.StartedAt = now
	s.status.StoppedAt = time.Time{}
	s.status.LastResumeID = strings.TrimSpace(resume.ThreadID)
	s.status.LastResumeLast = resume.Last
	s.mu.Unlock()
	return nil
}

// Stop asks the launched process to finish cleanly and waits for its actual
// exit before returning. The configured timeout is independent from the
// caller's context; the shorter deadline wins.
func (s *Supervisor) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	process := s.process
	startDone := s.startDone
	starting := s.starting
	if process == nil && starting {
		s.mu.Unlock()
		select {
		case <-startDone:
			return s.Stop(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if process == nil {
		s.mu.Unlock()
		return nil
	}
	s.status.State = StateStopping
	s.mu.Unlock()

	if err := process.Interrupt(); err != nil {
		// An interrupt failure is recoverable: the process may still honour a
		// termination request. Keep waiting and report only if escalation also
		// fails.
		_ = process.Terminate()
	}
	waitErr := waitForProcess(ctx, process, s.config.ShutdownTimeout)
	if waitErr == nil {
		s.finishStop(process, nil)
		return nil
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) && !errors.Is(waitErr, context.Canceled) {
		// The process exited with a non-zero status. A deliberate stop still
		// completed; retain the exit detail in diagnostics but do not turn a
		// normal signal exit into a failed restart.
		s.finishStop(process, waitErr)
		return nil
	}
	if err := process.Terminate(); err != nil {
		_ = process.Kill()
	}
	// A canceled caller still must not leave Codex running with an ambiguous
	// auth file. Once escalation starts, use a fresh bounded wait context.
	killErr := waitForProcess(context.Background(), process, s.config.KillTimeout)
	if killErr != nil {
		if isProcessDone(process) {
			s.finishStop(process, killErr)
			return nil
		}
		_ = process.Kill()
		finalErr := waitForProcess(context.Background(), process, s.config.KillTimeout)
		if finalErr != nil && !isProcessDone(process) {
			return fmt.Errorf("stop installed Codex: %w", finalErr)
		}
		if finalErr != nil {
			s.finishStop(process, finalErr)
		} else {
			s.finishStop(process, nil)
		}
		return nil
	}
	s.finishStop(process, nil)
	return nil
}

func (s *Supervisor) finishStop(process Process, exitErr error) {
	s.mu.Lock()
	if s.process == process {
		s.process = nil
		s.status.State = StateStopped
		s.status.PID = 0
		s.status.StoppedAt = time.Now().UTC()
		if exitErr != nil {
			s.status.LastExit = exitErr.Error()
		}
	}
	s.mu.Unlock()
}

// RestartAndResume performs the only safe fallback when the installed Codex
// process does not expose the live Marathon reload interface. It waits for
// the old process to exit, invokes deployCredentials, and starts `codex
// resume <thread-id>` (or `codex resume --last`). The callback must atomically
// deploy the selected auth snapshot and must not start Codex itself.
func (s *Supervisor) RestartAndResume(ctx context.Context, target ResumeTarget, deployCredentials func(context.Context) error) error {
	if s == nil {
		return ErrNilSupervisor
	}
	if err := target.validate(); err != nil {
		return err
	}
	if deployCredentials == nil {
		return errors.New("credential deployment callback is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.Stop(ctx); err != nil {
		return err
	}
	if err := deployCredentials(ctx); err != nil {
		s.mu.Lock()
		s.status.State = StateFailed
		s.status.LastError = fmt.Sprintf("deploy credentials before Codex restart: %v", err)
		s.mu.Unlock()
		return err
	}
	command, err := BuildResumeCommand(s.config.Command, target)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.RestartCount++
	s.mu.Unlock()
	workingDir := s.config.WorkingDir
	if strings.TrimSpace(target.WorkingDir) != "" {
		workingDir = target.WorkingDir
	}
	return s.start(command, workingDir, target)
}

// Wait waits for the currently tracked process. It is useful to let a CLI
// wrapper mirror Codex's lifetime. An intentional Stop returns nil; an
// unexpected process exit returns its wait error.
func (s *Supervisor) Wait(ctx context.Context) error {
	if s == nil {
		return ErrNilSupervisor
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.RLock()
	process := s.process
	s.mu.RUnlock()
	if process == nil {
		return ErrNotStarted
	}
	select {
	case <-process.Done():
		err := process.WaitError()
		s.mu.Lock()
		if s.process == process {
			s.process = nil
			s.status.PID = 0
			s.status.StoppedAt = time.Now().UTC()
			if err == nil {
				s.status.State = StateStopped
			} else {
				s.status.State = StateFailed
				s.status.LastExit = err.Error()
			}
		}
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForProcess(ctx context.Context, process Process, timeout time.Duration) error {
	if process == nil || process.Done() == nil {
		return errors.New("Codex process has no wait channel")
	}
	// The caller passes a context with the desired deadline. Stop creates a
	// bounded context for each escalation phase; keeping the process itself as
	// the input means observing exit is broadcast-safe.
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.Done():
		return process.WaitError()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func isProcessDone(process Process) bool {
	if process == nil || process.Done() == nil {
		return false
	}
	select {
	case <-process.Done():
		return true
	default:
		return false
	}
}

// ResolveInstalledCodex locates the user's installed Codex executable. It
// deliberately uses PATH discovery and does not fall back to a bundled
// codex-app-server binary.
func ResolveInstalledCodex() (string, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return "", fmt.Errorf("find installed Codex CLI: %w", err)
	}
	return path, nil
}

// IsCodexCommand performs a conservative executable-name check for callers
// that want to reject an accidental app-server path before starting it.
func IsCodexCommand(command string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	return base == "codex" || base == "codex.exe"
}

func isBundledRuntimeCommand(command string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	return base == "codex-app-server" ||
		base == "codex-app-server.exe" ||
		base == "codexmarathon-runtime" ||
		base == "codexmarathon-runtime.exe"
}
