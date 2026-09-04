package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/runtime"
	"codexmarathon/controller/internal/transitions"
	fakeruntime "codexmarathon/controller/integration/fake-runtime"
)

type memoryDeployer struct {
	mu       sync.Mutex
	disk     string
	deploys  []string
	err      error
}

func (d *memoryDeployer) Deploy(accountID string) (credentials.DeploymentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return credentials.DeploymentResult{}, d.err
	}
	d.disk = accountID
	d.deploys = append(d.deploys, accountID)
	return credentials.DeploymentResult{AccountID: accountID, Path: "memory-auth.json", DeployedAt: time.Now().UTC()}, nil
}

func (d *memoryDeployer) Disk() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.disk
}

func (d *memoryDeployer) DeployCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.deploys)
}

func TestSafeTransitionDefersUntilTurnBoundary(t *testing.T) {
	rt := fakeruntime.New("account-a", 1)
	rt.SetActiveTurnCount(1)
	deployer := &memoryDeployer{disk: "account-a"}
	coordinator := transitions.NewCoordinator(transitions.Config{
		Runtime: rt,
		Deployer: deployer,
		Disk: transitions.DiskIdentityFunc(func() (string, error) { return deployer.Disk(), nil }),
		BoundaryPollInterval: 5 * time.Millisecond,
	})

	resultCh := make(chan struct {
		result *transitions.TransitionResult
		err    error
	}, 1)
	go func() {
		result, err := coordinator.RequestTransition(context.Background(), "account-b")
		resultCh <- struct {
			result *transitions.TransitionResult
			err    error
		}{result, err}
	}()

	deadline := time.After(time.Second)
	for len(rt.PrepareCalls()) == 0 {
		select {
		case <-deadline:
			t.Fatal("transition was not prepared")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if deployer.DeployCount() != 0 {
		t.Fatal("credentials deployed during active turn")
	}
	rt.FinishTurn("turn-1")

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("RequestTransition() error = %v", result.err)
		}
		if result.result == nil || result.result.Outcome != transitions.TransitionCommitted {
			t.Fatalf("unexpected transition result: %#v", result.result)
		}
		if result.result.AdoptedAccountID != "account-b" || result.result.FinalGeneration != 2 {
			t.Fatalf("unexpected adopted identity: %#v", result.result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for safe transition")
	}
	if rt.ActiveTurnCount() != 0 || rt.Identity().AuthGeneration != 2 {
		t.Fatalf("runtime did not finish transition: %+v", rt.Identity())
	}
	if deployer.Disk() != "account-b" {
		t.Fatalf("disk identity = %q, want account-b", deployer.Disk())
	}
	prepareCalls, commitCalls := rt.PrepareCalls(), rt.CommitCalls()
	if len(prepareCalls) != 1 || len(commitCalls) != 1 {
		t.Fatalf("command counts prepare=%d commit=%d", len(prepareCalls), len(commitCalls))
	}
	if prepareCalls[0].TransitionID != commitCalls[0].TransitionID || prepareCalls[0].ExpectedGeneration != commitCalls[0].ExpectedGeneration {
		t.Fatal("prepare and commit did not preserve one transition correlation")
	}
}

func TestLostCommitAckReconcilesCommittedState(t *testing.T) {
	rt := fakeruntime.New("account-a", 7)
	deployer := &memoryDeployer{disk: "account-a"}
	coordinator := transitions.NewCoordinator(transitions.Config{
		Runtime: rt,
		Deployer: deployer,
		Disk: transitions.DiskIdentityFunc(func() (string, error) { return deployer.Disk(), nil }),
	})
	rt.DropNextCommitAck()
	result, err := coordinator.RequestTransition(context.Background(), "account-b")
	if result == nil || result.Outcome != transitions.TransitionUncertain {
		t.Fatalf("lost ACK should be uncertain, result=%#v err=%v", result, err)
	}
	if !errors.Is(err, fakeruntime.ErrAckLost) {
		t.Fatalf("lost ACK error = %v, want ErrAckLost", err)
	}
	if got := rt.Identity(); got.AuthGeneration != 8 || got.AccountID == nil || *got.AccountID != "account-b" {
		t.Fatalf("runtime state was not applied before lost ACK: %+v", got)
	}

	reconciled, err := coordinator.Reconcile(context.Background(), result.TransitionID)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if reconciled == nil || reconciled.Outcome != transitions.TransitionCommitted || reconciled.FinalGeneration != 8 {
		t.Fatalf("unexpected reconciled result: %#v", reconciled)
	}
	if len(rt.CommitCalls()) != 1 {
		t.Fatalf("reconcile reissued an unnecessary commit: %d calls", len(rt.CommitCalls()))
	}
	if _, pending := coordinator.Pending(); pending {
		t.Fatal("resolved transition still blocks new requests")
	}
}

func TestStaleGenerationRejectedWithoutApplyingAuth(t *testing.T) {
	rt := fakeruntime.New("account-a", 43)
	result, err := rt.CommitAuthTransition(context.Background(), runtime.AuthTransitionParams{
		TransitionID:       "stale",
		TargetAccountID:    "account-b",
		ExpectedGeneration: 42,
	})
	if err != nil {
		t.Fatalf("stale commit transport error = %v", err)
	}
	if result.Outcome != runtime.TransitionRejected || result.ErrorCode == nil || *result.ErrorCode != "stale_transition" {
		t.Fatalf("unexpected stale response: %#v", result)
	}
	identity := rt.Identity()
	if identity.AuthGeneration != 43 || identity.AccountID == nil || *identity.AccountID != "account-a" {
		t.Fatalf("stale command changed runtime identity: %+v", identity)
	}
}

func TestIdentityComparisonDistinguishesSplitBrainAndReload(t *testing.T) {
	base := transitions.IdentityObservation{
		DesiredAccountID:     "account-b",
		PreviousAccountID:    "account-a",
		ExpectedRuntimeID:    "runtime-1",
		RuntimeID:            "runtime-1",
		ControllerGeneration: 1,
		ExpectedGeneration:   2,
	}
	committed := base
	committed.DiskAccountID = "account-b"
	committed.RuntimeAccountID = "account-b"
	committed.RuntimeGeneration = 2
	if got := transitions.CompareIdentity(committed); got != transitions.ReconcileCommitted {
		t.Fatalf("matching state classified as %q", got)
	}
	reload := base
	reload.DiskAccountID = "account-b"
	reload.RuntimeAccountID = "account-a"
	reload.RuntimeGeneration = 1
	if got := transitions.CompareIdentity(reload); got != transitions.ReconcileRuntimeReload {
		t.Fatalf("disk-target/runtime-source classified as %q", got)
	}
	split := base
	split.DiskAccountID = "account-a"
	split.RuntimeAccountID = "account-b"
	split.RuntimeGeneration = 2
	if got := transitions.CompareIdentity(split); got != transitions.ReconcileSplitBrain {
		t.Fatalf("split-brain state classified as %q", got)
	}
}
