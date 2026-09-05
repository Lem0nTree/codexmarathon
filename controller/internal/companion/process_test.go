package companion

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeProcess struct {
	mu          sync.Mutex
	pid         int
	done        chan struct{}
	waitErr     error
	interrupts  int
	terminates  int
	kills       int
	ignoreUntil string
}

func newFakeProcess(pid int) *fakeProcess {
	return &fakeProcess{pid: pid, done: make(chan struct{})}
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Done() <-chan struct{} { return p.done }

func (p *fakeProcess) WaitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *fakeProcess) Interrupt() error {
	p.mu.Lock()
	p.interrupts++
	ignore := p.ignoreUntil == "interrupt"
	p.mu.Unlock()
	if !ignore {
		p.finish(nil)
	}
	return nil
}

func (p *fakeProcess) Terminate() error {
	p.mu.Lock()
	p.terminates++
	ignore := p.ignoreUntil == "terminate"
	p.mu.Unlock()
	if !ignore {
		p.finish(errors.New("terminated"))
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.kills++
	ignore := p.ignoreUntil == "kill"
	p.mu.Unlock()
	if !ignore {
		p.finish(errors.New("killed"))
	}
	return nil
}

func (p *fakeProcess) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
	}
	p.waitErr = err
	close(p.done)
}

func (p *fakeProcess) counts() (interrupts, terminates, kills int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupts, p.terminates, p.kills
}

type recordingFactory struct {
	mu        sync.Mutex
	commands  [][]string
	dirs      []string
	envs      [][]string
	processes []Process
}

func (f *recordingFactory) start(command []string, dir string, env []string) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), command...))
	f.dirs = append(f.dirs, dir)
	f.envs = append(f.envs, append([]string(nil), env...))
	if len(f.processes) == 0 {
		return nil, errors.New("test process queue is empty")
	}
	process := f.processes[0]
	f.processes = f.processes[1:]
	return process, nil
}

func (f *recordingFactory) snapshot() ([][]string, []string, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	commands := make([][]string, len(f.commands))
	for i := range f.commands {
		commands[i] = append([]string(nil), f.commands[i]...)
	}
	return commands, append([]string(nil), f.dirs...), append([][]string(nil), f.envs...)
}

func TestRestartAndResumeStopsBeforeCredentialDeployment(t *testing.T) {
	first := newFakeProcess(101)
	second := newFakeProcess(202)
	factory := &recordingFactory{processes: []Process{first, second}}
	supervisor, err := NewSupervisor(Config{
		Command:         []string{"codex"},
		ProcessFactory:  factory.start,
		ShutdownTimeout: time.Second,
		KillTimeout:     time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	deployed := false
	err = supervisor.RestartAndResume(context.Background(), ResumeTarget{ThreadID: "thread-123"}, func(context.Context) error {
		select {
		case <-first.Done():
		default:
			t.Fatal("credential deployment ran before the old Codex process exited")
		}
		deployed = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !deployed {
		t.Fatal("credential deployment callback was not called")
	}
	commands, _, _ := factory.snapshot()
	want := [][]string{{"codex"}, {"codex", "resume", "thread-123"}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	status := supervisor.Status()
	if status.State != StateRunning || status.PID != 202 || status.RestartCount != 1 {
		t.Fatalf("status = %#v, want running pid=202 restart_count=1", status)
	}
	if status.LastResumeID != "thread-123" || status.LastResumeLast {
		t.Fatalf("resume status = %#v, want exact thread target", status)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestartAndResumeUsesLastAndPreservesArgvBoundaries(t *testing.T) {
	first := newFakeProcess(101)
	second := newFakeProcess(202)
	factory := &recordingFactory{processes: []Process{first, second}}
	dir := t.TempDir()
	supervisor, err := NewSupervisor(Config{
		Command:         []string{"/usr/bin/codex", "--no-alt-screen"},
		WorkingDir:      dir,
		Environment:     []string{"CODEX_HOME=/tmp/codex-home"},
		ProcessFactory:  factory.start,
		ShutdownTimeout: time.Second,
		KillTimeout:     time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	resumeDir := t.TempDir()
	err = supervisor.RestartAndResume(context.Background(), ResumeTarget{
		Last:       true,
		WorkingDir: resumeDir,
		ExtraArgs:  []string{"--no-alt-screen", "--dangerously-bypass-approvals-and-sandbox"},
	}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	commands, dirs, envs := factory.snapshot()
	want := [][]string{
		{"/usr/bin/codex", "--no-alt-screen"},
		{"/usr/bin/codex", "--no-alt-screen", "resume", "--last", "--cd", resumeDir, "--no-alt-screen", "--dangerously-bypass-approvals-and-sandbox"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	if dirs[0] != dir || dirs[1] != resumeDir {
		t.Fatalf("working directories = %#v, want initial=%q resumed=%q", dirs, dir, resumeDir)
	}
	if len(envs) != 2 || !reflect.DeepEqual(envs[0], []string{"CODEX_HOME=/tmp/codex-home"}) {
		t.Fatalf("environment = %#v, want copied configured environment", envs)
	}
	status := supervisor.Status()
	if !status.LastResumeLast || status.LastResumeID != "" {
		t.Fatalf("resume status = %#v, want --last", status)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopEscalatesOnlyAfterGracefulDeadline(t *testing.T) {
	process := newFakeProcess(101)
	process.ignoreUntil = "interrupt"
	factory := &recordingFactory{processes: []Process{process}}
	supervisor, err := NewSupervisor(Config{
		Command:         []string{"codex"},
		ProcessFactory:  factory.start,
		ShutdownTimeout: 5 * time.Millisecond,
		KillTimeout:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	interrupts, terminates, kills := process.counts()
	if interrupts != 1 || terminates != 1 || kills != 0 {
		t.Fatalf("signals = interrupt:%d terminate:%d kill:%d, want 1/1/0", interrupts, terminates, kills)
	}
	if status := supervisor.Status(); status.State != StateStopped || status.PID != 0 {
		t.Fatalf("status after stop = %#v, want stopped without pid", status)
	}
}

func TestRestartAndResumeRequiresTargetAndCallback(t *testing.T) {
	first := newFakeProcess(101)
	factory := &recordingFactory{processes: []Process{first}}
	supervisor, err := NewSupervisor(Config{Command: []string{"codex"}, ProcessFactory: factory.start})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.RestartAndResume(context.Background(), ResumeTarget{}, func(context.Context) error { return nil }); !errors.Is(err, ErrResumeTargetMissing) {
		t.Fatalf("missing target error = %v, want ErrResumeTargetMissing", err)
	}
	if err := supervisor.RestartAndResume(context.Background(), ResumeTarget{Last: true}, nil); err == nil {
		t.Fatal("nil deployment callback error = nil, want error")
	}
	if status := supervisor.Status(); status.State != StateRunning || status.PID != 101 {
		t.Fatalf("status after validation failures = %#v, want original process running", status)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorRejectsBundledRuntimeCommand(t *testing.T) {
	for _, command := range []string{"codex-app-server", "/opt/codexmarathon-runtime", "codex-app-server.exe"} {
		if _, err := NewSupervisor(Config{Command: []string{command}}); err == nil {
			t.Fatalf("NewSupervisor(%q) error = nil, want bundled runtime rejection", command)
		}
	}
}

func TestWaitBroadcastsOneProcessExitToStopAndWait(t *testing.T) {
	process := newFakeProcess(101)
	factory := &recordingFactory{processes: []Process{process}}
	supervisor, err := NewSupervisor(Config{Command: []string{"codex"}, ProcessFactory: factory.start})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	process.finish(errors.New("unexpected exit"))
	waitErr := supervisor.Wait(context.Background())
	if waitErr == nil || waitErr.Error() != "unexpected exit" {
		t.Fatalf("Wait() error = %v, want unexpected exit", waitErr)
	}
	if status := supervisor.Status(); status.State != StateFailed || status.PID != 0 {
		t.Fatalf("status after unexpected exit = %#v, want failed without pid", status)
	}
}
