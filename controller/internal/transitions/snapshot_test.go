package transitions

import (
	"context"
	"encoding/json"
	"testing"

	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/runtime"
)

type snapshotRuntimeFixture struct {
	identity runtime.Identity
	snapshot runtime.NativeAuthSnapshotResult
}

func (f *snapshotRuntimeFixture) GetRuntimeState(context.Context) (runtime.RuntimeState, error) {
	return runtime.RuntimeState{
		Identity:        f.identity,
		ActiveTurnCount: 0,
	}, nil
}

func (f *snapshotRuntimeFixture) PrepareAuthTransition(_ context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          f.identity.AccountID,
	}, nil
}

func (f *snapshotRuntimeFixture) CommitAuthTransition(_ context.Context, params runtime.AuthTransitionParams) (runtime.TransitionResult, error) {
	f.identity.AccountID = stringPointer(params.TargetAccountID)
	f.identity.AuthGeneration = params.ExpectedGeneration
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          f.identity.AccountID,
	}, nil
}

func (f *snapshotRuntimeFixture) CancelAuthTransition(_ context.Context, params runtime.CancelAuthTransitionParams) (runtime.TransitionResult, error) {
	return runtime.TransitionResult{
		TransitionID:       params.TransitionID,
		RuntimeID:          f.identity.RuntimeID,
		ExpectedGeneration: params.ExpectedGeneration,
		AuthGeneration:     f.identity.AuthGeneration,
		Outcome:            runtime.TransitionCommitted,
		AccountID:          f.identity.AccountID,
	}, nil
}

func (f *snapshotRuntimeFixture) GetIdentity(context.Context) (runtime.Identity, error) {
	return f.identity, nil
}

func (f *snapshotRuntimeFixture) GetAuthGeneration(context.Context) (runtime.AuthGenerationResult, error) {
	return runtime.AuthGenerationResult{AuthGeneration: f.identity.AuthGeneration}, nil
}

func (f *snapshotRuntimeFixture) ReadAuthSnapshot(context.Context) (runtime.NativeAuthSnapshotResult, error) {
	return f.snapshot, nil
}

type snapshotWriterFixture struct {
	accountID string
	raw       []byte
}

func (w *snapshotWriterFixture) Save(accountID string, raw []byte) error {
	w.accountID = accountID
	w.raw = append([]byte(nil), raw...)
	return nil
}

type snapshotDeployerFixture struct {
	accountID string
}

func (d *snapshotDeployerFixture) Deploy(accountID string) (credentials.DeploymentResult, error) {
	d.accountID = accountID
	return credentials.DeploymentResult{AccountID: accountID}, nil
}

func stringPointer(value string) *string { return &value }

func TestCoordinatorSynchronizesNativeSnapshotBeforeDeployment(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens":    map[string]string{"account_id": "account-a", "access_token": "refreshed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := &snapshotRuntimeFixture{
		identity: runtime.Identity{
			RuntimeID:      "runtime-test",
			AccountID:      stringPointer("account-a"),
			AuthGeneration: 1,
		},
		snapshot: runtime.NativeAuthSnapshotResult{
			AccountID: "account-a",
			AuthJSON:  raw,
		},
	}
	writer := &snapshotWriterFixture{}
	deployer := &snapshotDeployerFixture{}
	coordinator := NewCoordinator(Config{
		Runtime:        rt,
		Deployer:       deployer,
		SnapshotWriter: writer,
	})

	result, err := coordinator.RequestTransition(context.Background(), "account-b")
	if err != nil {
		t.Fatalf("RequestTransition() error = %v", err)
	}
	if result.Outcome != TransitionCommitted {
		t.Fatalf("RequestTransition() outcome = %q, want committed", result.Outcome)
	}
	if writer.accountID != "account-a" || string(writer.raw) != string(raw) {
		t.Fatalf("snapshot writer = (%q, %q), want Account A opaque snapshot", writer.accountID, writer.raw)
	}
	if deployer.accountID != "account-b" {
		t.Fatalf("deployer account = %q, want account-b", deployer.accountID)
	}
}
