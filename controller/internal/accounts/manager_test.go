package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexmarathon/controller/internal/credentials"
)

type fakeAuthService struct {
	loginResults  []LoginResult
	loginErr      error
	refreshTokens credentials.TokenSet
	refreshErr    error
	lastRefresh   RefreshRequest
}

func (f *fakeAuthService) Login(context.Context, LoginRequest) (LoginResult, error) {
	if f.loginErr != nil {
		return LoginResult{}, f.loginErr
	}
	if len(f.loginResults) == 0 {
		return LoginResult{}, errors.New("no fake login result")
	}
	result := f.loginResults[0]
	f.loginResults = f.loginResults[1:]
	return result, nil
}

func (f *fakeAuthService) Refresh(_ context.Context, request RefreshRequest) (credentials.TokenSet, error) {
	f.lastRefresh = RefreshRequest{AccountID: request.AccountID, AuthJSON: append([]byte(nil), request.AuthJSON...)}
	if f.refreshErr != nil {
		return credentials.TokenSet{}, f.refreshErr
	}
	return f.refreshTokens, nil
}

type failingDeployer struct {
	vault    credentials.SnapshotReader
	authPath string
	failID   string
}

func (d failingDeployer) Deploy(accountID string) (credentials.DeploymentResult, error) {
	if accountID == d.failID {
		return credentials.DeploymentResult{}, errors.New("simulated deployment failure")
	}
	raw, err := d.vault.Load(accountID)
	if err != nil {
		return credentials.DeploymentResult{}, err
	}
	if err := credentials.WriteAtomically(d.authPath, raw); err != nil {
		return credentials.DeploymentResult{}, err
	}
	return credentials.DeploymentResult{AccountID: accountID, Path: d.authPath}, nil
}

func newManagerFixture(t *testing.T, auth AuthService, deployer credentials.CredentialDeployer) (*Manager, *credentials.FileVault, *FileRegistry, string) {
	t.Helper()
	root := t.TempDir()
	vault := credentials.NewFileVault(filepath.Join(root, "credentials"))
	registry := NewFileRegistry(filepath.Join(root, "accounts.json"))
	authPath := filepath.Join(root, "codex", "auth.json")
	manager := NewManager(ManagerConfig{
		Registry:    registry,
		Vault:       vault,
		Deployer:    deployer,
		AuthPath:    authPath,
		AuthService: auth,
		Now:         func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) },
	})
	if manager.deployer == nil {
		manager.deployer = credentials.NewAtomicDeployer(vault, authPath)
	}
	return manager, vault, registry, authPath
}

func loginSnapshot(accountID, token string) []byte {
	return []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"` + token + `","id_token":"id-` + accountID + `","refresh_token":"refresh-` + accountID + `","account_id":"` + accountID + `"},"custom":{"keep":true}}`)
}

func TestManagerLoginSwitchRefreshAndRenameLifecycle(t *testing.T) {
	service := &fakeAuthService{
		loginResults: []LoginResult{
			{AccountID: "acct-a", Alias: "Personal", AuthJSON: loginSnapshot("acct-a", "token-a"), Metadata: map[string]string{"plan": "plus"}},
			{AccountID: "acct-b", Alias: "Work", AuthJSON: loginSnapshot("acct-b", "token-b")},
		},
	}
	manager, vault, registry, authPath := newManagerFixture(t, service, nil)

	first, err := manager.Login(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatalf("first Login() error = %v", err)
	}
	if !first.Activated || first.AccountID != "acct-a" {
		t.Fatalf("first Login() = %#v, want activated acct-a", first)
	}
	firstAccount, err := registry.Get("acct-a")
	if err != nil {
		t.Fatal(err)
	}
	if firstAccount.Metadata["plan"] != "plus" {
		t.Fatalf("native login metadata = %#v, want plan marker", firstAccount.Metadata)
	}
	if got := readAuthIdentity(t, authPath); got != "acct-a" {
		t.Fatalf("initial auth identity = %q, want acct-a", got)
	}

	second, err := manager.Login(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatalf("second Login() error = %v", err)
	}
	if second.Activated {
		t.Fatalf("second Login() = %#v, want inactive by default", second)
	}
	active, ok, err := registry.Active()
	if err != nil || !ok || active.ID != "acct-a" {
		t.Fatalf("active after second login = %#v, %v, %v", active, ok, err)
	}

	if _, err := manager.Activate(context.Background(), "acct-b"); err != nil {
		t.Fatalf("Activate(acct-b) error = %v", err)
	}
	if got := readAuthIdentity(t, authPath); got != "acct-b" {
		t.Fatalf("auth identity after B = %q, want acct-b", got)
	}
	active, ok, err = registry.Active()
	if err != nil || !ok || active.ID != "acct-b" {
		t.Fatalf("active after B = %#v, %v, %v", active, ok, err)
	}

	service.refreshTokens = credentials.TokenSet{AccessToken: "token-b-refreshed", AccountID: "acct-b"}
	refresh, err := manager.Refresh(context.Background(), "acct-b")
	if err != nil {
		t.Fatalf("Refresh(B) error = %v", err)
	}
	if !refresh.Activated || service.lastRefresh.AccountID != "acct-b" {
		t.Fatalf("Refresh(B) = %#v, request=%#v", refresh, service.lastRefresh)
	}
	updatedB, err := vault.Load("acct-b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updatedB), `"access_token":"token-b-refreshed"`) || !strings.Contains(string(updatedB), `"keep":true`) {
		t.Fatalf("refreshed B snapshot lost fields: %s", updatedB)
	}

	if _, err := manager.Activate(context.Background(), "acct-a"); err != nil {
		t.Fatalf("Activate(acct-a) error = %v", err)
	}
	service.refreshTokens = credentials.TokenSet{AccessToken: "token-a-refreshed", AccountID: "acct-a"}
	if _, err := manager.Refresh(context.Background(), "acct-a"); err != nil {
		t.Fatalf("Refresh(A) error = %v", err)
	}
	if got := readAuthIdentity(t, authPath); got != "acct-a" {
		t.Fatalf("auth identity after A = %q, want acct-a", got)
	}
	updatedA, err := vault.Load("acct-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updatedA), `"access_token":"token-a-refreshed"`) {
		t.Fatalf("refreshed A snapshot missing new token: %s", updatedA)
	}

	if err := manager.Rename("acct-a", "Personal main"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	status, err := manager.Status("acct-a")
	if err != nil {
		t.Fatal(err)
	}
	if status.Alias != "Personal main" || !status.Active || !status.CredentialPresent {
		t.Fatalf("status after rename = %#v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "token-a") || strings.Contains(string(encoded), "refresh-") {
		t.Fatalf("profile status exposed credential data: %s", encoded)
	}
}

func TestManagerImportExistingSnapshotWithoutAuthService(t *testing.T) {
	manager, vault, registry, authPath := newManagerFixture(t, nil, nil)
	raw := loginSnapshot("stock-account", "stock-token")

	outcome, err := manager.Import(context.Background(), ImportRequest{
		AuthJSON: raw,
		Alias:    "personal",
	})
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if !outcome.Activated || outcome.AccountID != "stock-account" || outcome.Alias != "personal" {
		t.Fatalf("Import() = %#v, want activated stock-account/personal", outcome)
	}
	stored, err := vault.Load("stock-account")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(raw) {
		t.Fatalf("stored snapshot changed during import: got %s want %s", stored, raw)
	}
	deployed := mustReadFile(t, authPath)
	if string(deployed) != string(raw) {
		t.Fatalf("deployed snapshot changed during import: got %s want %s", deployed, raw)
	}
	account, err := registry.Get("stock-account")
	if err != nil {
		t.Fatal(err)
	}
	if account.Alias != "personal" || account.CredentialRef != "stock-account" {
		t.Fatalf("imported account = %#v", account)
	}
}

func TestManagerImportRejectsSnapshotIdentityMismatch(t *testing.T) {
	manager, _, _, _ := newManagerFixture(t, nil, nil)
	_, err := manager.Import(context.Background(), ImportRequest{
		AccountID: "requested-account",
		AuthJSON:  loginSnapshot("snapshot-account", "stock-token"),
	})
	if !errors.Is(err, ErrLoginIdentityMismatch) {
		t.Fatalf("Import() error = %v, want ErrLoginIdentityMismatch", err)
	}
}

func TestManagerOverwriteOfActiveProfileRedeploysNewSnapshot(t *testing.T) {
	service := &fakeAuthService{
		loginResults: []LoginResult{
			{AccountID: "acct-a", AuthJSON: loginSnapshot("acct-a", "token-a")},
			{AccountID: "acct-a", AuthJSON: loginSnapshot("acct-a", "token-a-new")},
		},
	}
	manager, vault, _, authPath := newManagerFixture(t, service, nil)
	if _, err := manager.Login(context.Background(), LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	overwrite, err := manager.Login(context.Background(), LoginRequest{Overwrite: true})
	if err != nil {
		t.Fatalf("overwrite Login() error = %v", err)
	}
	if !overwrite.Activated {
		t.Fatalf("overwrite Login() = %#v, want active redeployment", overwrite)
	}
	if got := readAuthIdentity(t, authPath); got != "acct-a" {
		t.Fatalf("auth identity after overwrite = %q, want acct-a", got)
	}
	raw, err := vault.Load("acct-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "token-a-new") {
		t.Fatalf("vault after overwrite does not contain new snapshot: %s", raw)
	}
	if !strings.Contains(string(mustReadFile(t, authPath)), "token-a-new") {
		t.Fatalf("auth.json after overwrite does not contain new snapshot")
	}
}

func TestManagerActivationFailureAndCancellationPreservePreviousActive(t *testing.T) {
	service := &fakeAuthService{
		loginResults: []LoginResult{
			{AccountID: "acct-a", AuthJSON: loginSnapshot("acct-a", "token-a")},
			{AccountID: "acct-b", AuthJSON: loginSnapshot("acct-b", "token-b")},
		},
	}
	manager, vault, registry, authPath := newManagerFixture(t, service, nil)
	if _, err := manager.Login(context.Background(), LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Login(context.Background(), LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	manager.deployer = failingDeployer{vault: vault, authPath: authPath, failID: "acct-b"}
	if _, err := manager.Activate(context.Background(), "acct-b"); err == nil {
		t.Fatal("Activate(B) error = nil, want failure")
	}
	active, ok, err := registry.Active()
	if err != nil || !ok || active.ID != "acct-a" {
		t.Fatalf("active after failed activation = %#v, %v, %v", active, ok, err)
	}
	if got := readAuthIdentity(t, authPath); got != "acct-a" {
		t.Fatalf("auth after failed activation = %q, want acct-a", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Activate(ctx, "acct-b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Activate() error = %v, want context.Canceled", err)
	}
	active, ok, err = registry.Active()
	if err != nil || !ok || active.ID != "acct-a" {
		t.Fatalf("active after cancelled activation = %#v, %v, %v", active, ok, err)
	}
}

func TestManagerRejectsProviderErrorsWithoutLeakingSecrets(t *testing.T) {
	service := &fakeAuthService{loginErr: errors.New(`refresh_token="super-secret"`)}
	manager, _, _, _ := newManagerFixture(t, service, nil)
	_, err := manager.Login(context.Background(), LoginRequest{AccountID: "acct-a"})
	if !errors.Is(err, ErrNativeLoginFailed) {
		t.Fatalf("Login() error = %v, want ErrNativeLoginFailed", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("Login() leaked provider error: %v", err)
	}
}

func TestManagerSafeRemovalAndForceMarkerClearing(t *testing.T) {
	service := &fakeAuthService{loginResults: []LoginResult{{AccountID: "acct-a", AuthJSON: loginSnapshot("acct-a", "token-a")}, {AccountID: "acct-b", AuthJSON: loginSnapshot("acct-b", "token-b")}}}
	manager, _, registry, authPath := newManagerFixture(t, service, nil)
	if _, err := manager.Login(context.Background(), LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Login(context.Background(), LoginRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove("acct-a", false); !errors.Is(err, ErrActiveAccount) {
		t.Fatalf("Remove(active) error = %v, want ErrActiveAccount", err)
	}
	if err := manager.Remove("acct-b", false); err != nil {
		t.Fatalf("Remove(inactive) error = %v", err)
	}
	if err := manager.Remove("acct-a", true); err != nil {
		t.Fatalf("Remove(active, force) error = %v", err)
	}
	if _, ok, err := registry.Active(); err != nil || ok {
		t.Fatalf("active after forced removal = ok %v, err %v", ok, err)
	}
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("forced removal should preserve deployed auth.json: %v", err)
	}
}

func readAuthIdentity(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := credentials.ExtractAccountID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
