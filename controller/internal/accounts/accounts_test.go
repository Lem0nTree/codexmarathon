package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRegistryLifecycleAndDefensiveCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "accounts.json")
	registry := NewFileRegistry(path)
	clock := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	registry.SetClock(func() time.Time { return clock })
	account := Account{ID: "acct-b", Alias: "Work", Metadata: map[string]string{"plan": "plus"}}
	if err := registry.Register(account); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.Register(account); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("duplicate Register() error = %v, want ErrAccountExists", err)
	}
	if err := registry.Upsert(Account{ID: "acct-a", Alias: "Personal"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := registry.SetActive("acct-b"); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	active, ok, err := registry.Active()
	if err != nil || !ok {
		t.Fatalf("Active() = %#v, %v, %v", active, ok, err)
	}
	if active.CredentialRef != "acct-b" || active.CredentialHealth != CredentialHealthUnknown {
		t.Fatalf("active account defaults = %#v", active)
	}
	active.Metadata["mutated"] = "caller"
	loaded, err := registry.Get("acct-b")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, exists := loaded.Metadata["mutated"]; exists {
		t.Fatal("Get() returned mutable registry metadata")
	}
	accounts, err := registry.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 2 || accounts[0].ID != "acct-a" || accounts[1].ID != "acct-b" {
		t.Fatalf("List() = %#v, want ID-sorted accounts", accounts)
	}
	if !loaded.CreatedAt.Equal(clock) || !loaded.UpdatedAt.Equal(clock) {
		t.Fatalf("timestamps = %v, %v, want %v", loaded.CreatedAt, loaded.UpdatedAt, clock)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("registry Stat() error = %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("registry permissions = %o, want owner-only", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("registry directory Stat() error = %v", err)
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("registry directory permissions = %o, want owner-only", dirInfo.Mode().Perm())
	}
}

func TestFileRegistryActiveRemovalAndTelemetryMarkers(t *testing.T) {
	registry := NewFileRegistry(filepath.Join(t.TempDir(), "accounts.json"))
	if err := registry.Register(Account{ID: "acct-1"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetActive("acct-1"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Remove("acct-1"); !errors.Is(err, ErrActiveAccount) {
		t.Fatalf("Remove(active) error = %v, want ErrActiveAccount", err)
	}
	observed := time.Date(2026, 9, 4, 13, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	if err := registry.SetTelemetry("acct-1", observed, "runtime"); err != nil {
		t.Fatalf("SetTelemetry() error = %v", err)
	}
	if err := registry.SetCredentialHealth("acct-1", CredentialHealthHealthy); err != nil {
		t.Fatalf("SetCredentialHealth() error = %v", err)
	}
	account, err := registry.Get("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if account.LastTelemetryAt == nil || !account.LastTelemetryAt.Equal(observed.UTC()) || account.TelemetrySource != "runtime" || account.CredentialHealth != CredentialHealthHealthy {
		t.Fatalf("markers = %#v", account)
	}
	if err := registry.Remove("acct-1", true); err != nil {
		t.Fatalf("forced Remove() error = %v", err)
	}
	if _, ok, err := registry.Lookup("acct-1"); err != nil || ok {
		t.Fatalf("Lookup(deleted) = ok %v, error %v", ok, err)
	}
	if _, err := registry.ActiveID(); err != nil {
		t.Fatal(err)
	}
}

func TestFileRegistryRenamePreservesStableIdentityAndSelection(t *testing.T) {
	registry := NewFileRegistry(filepath.Join(t.TempDir(), "accounts.json"))
	if err := registry.Register(Account{ID: "acct-1", Alias: "old", CredentialRef: "acct-1"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetActive("acct-1"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Rename("acct-1", "new"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	account, err := registry.Get("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "acct-1" || account.Alias != "new" || account.CredentialRef != "acct-1" {
		t.Fatalf("renamed account = %#v", account)
	}
	activeID, err := registry.ActiveID()
	if err != nil || activeID != "acct-1" {
		t.Fatalf("active ID after rename = %q, error %v", activeID, err)
	}
}

func TestValidateAccountRejectsUnsafeIDsAndMetadata(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", `nested\name`, "nested/name", "line\nfeed"} {
		if err := ValidateAccount(Account{ID: id}); !errors.Is(err, ErrInvalidAccountID) {
			t.Fatalf("ValidateAccount(%q) error = %v, want ErrInvalidAccountID", id, err)
		}
	}
	if err := ValidateAccount(Account{ID: "acct", Metadata: map[string]string{"bad\nkey": "x"}}); !errors.Is(err, ErrInvalidRegistry) {
		t.Fatalf("invalid metadata error = %v, want ErrInvalidRegistry", err)
	}
}
