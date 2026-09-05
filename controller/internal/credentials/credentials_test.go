package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileVaultRoundTripListAndValidation(t *testing.T) {
	vault := NewFileVault(filepath.Join(t.TempDir(), "vault"))
	alpha := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"alpha","account_id":"acct-alpha"},"custom":{"enabled":true}}`)
	if err := vault.Save("acct-alpha", alpha); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := vault.Load("acct-alpha")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.Contains(string(got), `"access_token":"alpha"`) {
		t.Fatalf("Load() lost token field: %s", got)
	}
	got[0] = 'x'
	again, err := vault.Load("acct-alpha")
	if err != nil {
		t.Fatalf("Load() after mutation error = %v", err)
	}
	if again[0] == 'x' {
		t.Fatal("Load() returned mutable vault-owned bytes")
	}
	accounts, err := vault.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0] != "acct-alpha" {
		t.Fatalf("List() = %v, want [acct-alpha]", accounts)
	}

	info, err := os.Stat(filepath.Join(vault.Dir(), "acct-alpha.json"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("snapshot permissions = %o, want owner-only", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(vault.Dir())
	if err != nil {
		t.Fatalf("Stat(vault dir) error = %v", err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("vault directory permissions = %o, want owner-only", dirInfo.Mode().Perm())
	}

	if err := vault.Save("other", []byte(`{"tokens":{"account_id":"acct-alpha"}}`)); !errors.Is(err, ErrCredentialAccountMismatch) {
		t.Fatalf("Save(mismatched) error = %v, want ErrCredentialAccountMismatch", err)
	}
	for _, id := range []string{"", ".", "..", "../escape", `nested/name`, `nested\\name`} {
		if err := vault.Save(id, []byte(`{}`)); !errors.Is(err, ErrInvalidAccountID) {
			t.Fatalf("Save(%q) error = %v, want ErrInvalidAccountID", id, err)
		}
	}
}

func TestFileVaultWriteBackPreservesUnknownFieldsAndSupportsPartialTokens(t *testing.T) {
	vault := NewFileVault(filepath.Join(t.TempDir(), "vault"))
	raw := []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"old","id_token":"old-id","refresh_token":"old-refresh","account_id":"acct-1"},"custom":{"enabled":true}}`)
	if err := vault.Save("acct-1", raw); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	refreshed := time.Date(2026, 9, 4, 10, 11, 12, 345678000, time.FixedZone("UTC+2", 2*60*60))
	if err := vault.WriteBack(TokenWriteBackRequest{
		AccountID:   "acct-1",
		Tokens:      TokenSet{AccessToken: "new-access"},
		RefreshedAt: refreshed,
	}); err != nil {
		t.Fatalf("WriteBack() error = %v", err)
	}
	got, err := vault.Load("acct-1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	text := string(got)
	for _, want := range []string{`"access_token":"new-access"`, `"id_token":"old-id"`, `"refresh_token":"old-refresh"`, `"account_id":"acct-1"`, `"enabled":true`, `"last_refresh":"2026-09-04T08:11:12.345678Z"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("write-back document missing %s: %s", want, text)
		}
	}
	if strings.Contains(text, "new-refresh") {
		t.Fatal("partial write-back unexpectedly invented a refresh token")
	}
}

func TestMergeTokenWriteBackRejectsInvalidAndEmptyUpdates(t *testing.T) {
	if _, err := MergeTokenWriteBack([]byte(`{"tokens":`), TokenSet{AccessToken: "x"}, time.Now()); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("malformed merge error = %v, want ErrInvalidCredential", err)
	}
	if _, err := MergeTokenWriteBack([]byte(`{}`), TokenSet{}, time.Now()); !errors.Is(err, ErrEmptyTokenUpdate) {
		t.Fatalf("empty merge error = %v, want ErrEmptyTokenUpdate", err)
	}
}

func TestFileVaultRejectsConflictingAccountIdentities(t *testing.T) {
	vault := NewFileVault(filepath.Join(t.TempDir(), "vault"))
	raw := []byte(`{"account_id":"acct-a","tokens":{"account_id":"acct-b"}}`)
	if err := vault.Save("acct-a", raw); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Save(conflicting identities) error = %v, want ErrInvalidCredential", err)
	}
}

func TestAtomicDeployerReplacesDestinationAndCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	vault := NewFileVault(filepath.Join(dir, "vault"))
	if err := vault.Save("acct-1", []byte(`{"tokens":{"access_token":"new","account_id":"acct-1"}}`)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	authPath := filepath.Join(dir, "codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	deployer := NewAtomicDeployer(vault, authPath)
	deployedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	deployer.SetClock(func() time.Time { return deployedAt })
	result, err := deployer.Deploy("acct-1")
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if result.AccountID != "acct-1" || !result.DeployedAt.Equal(deployedAt) {
		t.Fatalf("DeploymentResult = %#v", result)
	}
	deployed, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deployed), `"access_token":"new"`) {
		t.Fatalf("auth.json = %s, want deployed snapshot", deployed)
	}
	entries, err := os.ReadDir(filepath.Dir(authPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".codexmarathon-auth-") {
			t.Fatalf("temporary deployment file %q remains", entry.Name())
		}
	}

	failingTarget := filepath.Join(dir, "codex", "target-dir")
	if err := os.Mkdir(failingTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	deployer.AuthPath = failingTarget
	if err := deployer.DeployRaw("acct-1", []byte(`{"tokens":{"access_token":"new","account_id":"acct-1"}}`)); err == nil {
		t.Fatal("DeployRaw(directory) error = nil, want replacement failure")
	}
	entries, err = os.ReadDir(filepath.Dir(failingTarget))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".codexmarathon-auth-") {
			t.Fatalf("temporary file %q remains after failure", entry.Name())
		}
	}
}

func TestFileVaultDeleteAndNotFound(t *testing.T) {
	vault := NewFileVault(filepath.Join(t.TempDir(), "vault"))
	if err := vault.Save("acct-1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := vault.Delete("acct-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := vault.Load("acct-1"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("Load(deleted) error = %v, want ErrCredentialNotFound", err)
	}
	if err := vault.Delete("acct-1"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("Delete(deleted) error = %v, want ErrCredentialNotFound", err)
	}
}
