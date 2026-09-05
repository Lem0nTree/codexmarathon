package app

import (
	"context"

	"codexmarathon/controller/internal/companion"
)

// CompanionConfig configures a CodexMarathon wrapper around an already
// installed Codex CLI. Command must point at that CLI; no app-server or
// Marathon runtime is started by this supervisor.
type CompanionConfig = companion.Config

// CompanionResumeTarget identifies the thread to reopen after an auth
// transition. Set ThreadID for an exact conversation or Last to use Codex's
// native most-recent-session selection.
type CompanionResumeTarget = companion.ResumeTarget

// CompanionStatus is a secret-free process lifecycle snapshot.
type CompanionStatus = companion.Status

const (
	CompanionStopped  = companion.StateStopped
	CompanionStarting = companion.StateStarting
	CompanionRunning  = companion.StateRunning
	CompanionStopping = companion.StateStopping
	CompanionFailed   = companion.StateFailed
)

var (
	ErrCompanionAlreadyStarted = companion.ErrAlreadyStarted
	ErrCompanionNotStarted     = companion.ErrNotStarted
	ErrCompanionResumeMissing  = companion.ErrResumeTargetMissing
	ErrCompanionResumeInvalid  = companion.ErrResumeTargetInvalid
)

// CompanionSupervisor manages only the Codex process it launched. It is the
// controlled-restart fallback for installations whose Codex version does not
// expose the live Marathon auth-reload protocol.
type CompanionSupervisor struct {
	supervisor *companion.Supervisor
}

// NewCompanionSupervisor validates a companion configuration without starting
// Codex.
func NewCompanionSupervisor(config CompanionConfig) (*CompanionSupervisor, error) {
	supervisor, err := companion.NewSupervisor(config)
	if err != nil {
		return nil, err
	}
	return &CompanionSupervisor{supervisor: supervisor}, nil
}

// ResolveInstalledCodex locates the user's installed Codex executable on PATH.
func ResolveInstalledCodex() (string, error) { return companion.ResolveInstalledCodex() }

// IsCodexCommand conservatively checks an executable basename.
func IsCodexCommand(command string) bool { return companion.IsCodexCommand(command) }

// BuildCompanionResumeCommand constructs the argv passed to the installed
// Codex CLI for a controlled conversation resume.
func BuildCompanionResumeCommand(command []string, target CompanionResumeTarget) ([]string, error) {
	return companion.BuildResumeCommand(command, target)
}

func (s *CompanionSupervisor) Start() error {
	if s == nil {
		return companion.ErrNilSupervisor
	}
	return s.supervisor.Start()
}

func (s *CompanionSupervisor) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.supervisor.Stop(ctx)
}

// RestartAndResume waits for the old Codex process to exit, invokes the
// caller's atomic credential deployment hook, and launches the installed CLI
// with `resume <thread-id>` or `resume --last`.
func (s *CompanionSupervisor) RestartAndResume(ctx context.Context, target CompanionResumeTarget, deployCredentials func(context.Context) error) error {
	if s == nil {
		return companion.ErrNilSupervisor
	}
	return s.supervisor.RestartAndResume(ctx, target, deployCredentials)
}

func (s *CompanionSupervisor) Wait(ctx context.Context) error {
	if s == nil {
		return companion.ErrNilSupervisor
	}
	return s.supervisor.Wait(ctx)
}

func (s *CompanionSupervisor) Status() CompanionStatus {
	if s == nil {
		return CompanionStatus{State: companion.StateFailed, LastError: companion.ErrNilSupervisor.Error()}
	}
	return s.supervisor.Status()
}
