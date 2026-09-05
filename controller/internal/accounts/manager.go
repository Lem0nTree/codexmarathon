package accounts

// This file is the controller-side account/profile manager.  It adapts the
// profile, refresh, atomic-write, and active-profile rules from the pinned
// humeo/codex-switch donor to CodexMarathon's registry/vault abstractions.
// Authentication itself is deliberately supplied by AuthService: the
// integrated Codex runtime owns OAuth, token parsing, browser/device login,
// and provider-specific refresh.  No shell command or external Codex
// installation is used here.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"codexmarathon/controller/internal/credentials"
)

var (
	// ErrAuthServiceUnavailable means the runtime has not yet supplied its
	// native Codex login/refresh implementation.
	ErrAuthServiceUnavailable = errors.New("native Codex authentication service is unavailable")
	// ErrLoginMissingIdentity means a login result contained neither an
	// account ID nor a caller-supplied stable ID.
	ErrLoginMissingIdentity = errors.New("login did not provide an account identity")
	// ErrLoginIdentityMismatch prevents a provider result from being stored
	// under a different account's stable ID.
	ErrLoginIdentityMismatch = errors.New("login account identity mismatch")
	// ErrCredentialStateMismatch indicates a registry/vault inconsistency
	// that should be repaired before attempting a lifecycle mutation.
	ErrCredentialStateMismatch = errors.New("account credential state is inconsistent")
	// ErrNativeLoginFailed and ErrNativeRefreshFailed intentionally hide the
	// native provider's free-form error text. Provider errors can accidentally
	// contain bearer material; cancellation is preserved separately below.
	ErrNativeLoginFailed   = errors.New("native login failed")
	ErrNativeRefreshFailed = errors.New("native refresh failed")
)

// LoginRequest contains only non-secret operator intent.  The integrated
// runtime may use Alias as a display hint, but it remains the source of the
// authenticated identity returned in LoginResult.
type LoginRequest struct {
	// AccountID is optional for OAuth login.  When omitted, the native runtime
	// must return the provider account ID in LoginResult.
	AccountID string
	Alias     string
	// Activate makes this account the controller-selected active profile after
	// the snapshot has been durably saved.  The first account is activated even
	// when this flag is false.
	Activate  bool
	Overwrite bool
	Metadata  map[string]string
}

// LoginResult is the in-memory handoff from the integrated runtime's native
// AuthManager/login crate.  AuthJSON is sensitive and is never returned by
// AccountManager methods, journalled, or printed; it exists only long enough
// to validate and persist the opaque snapshot into the vault.
type LoginResult struct {
	AccountID string
	Alias     string
	AuthJSON  []byte
	Metadata  map[string]string
}

// RefreshRequest is the in-memory handoff for a native refresh.  The runtime
// receives the opaque current snapshot so it can use its own auth schema and
// refresh implementation.  The manager validates the returned token set and
// merges it without discarding unknown auth fields.
type RefreshRequest struct {
	AccountID string
	AuthJSON  []byte
}

// AuthService is the direct integration seam for the embedded Codex login
// runtime.  Implementations should call Codex's native login/AuthManager
// APIs in-process (or through CodexMarathon's authenticated internal runtime
// boundary), never execute `codex login`, and never log the request/response
// token material.
type AuthService interface {
	Login(context.Context, LoginRequest) (LoginResult, error)
	Refresh(context.Context, RefreshRequest) (credentials.TokenSet, error)
}

// AuthServiceFunc is useful for wiring an integrated runtime and for tests
// without coupling the manager to a concrete Rust or RPC implementation.
type AuthServiceFunc struct {
	LoginFunc   func(context.Context, LoginRequest) (LoginResult, error)
	RefreshFunc func(context.Context, RefreshRequest) (credentials.TokenSet, error)
}

func (f AuthServiceFunc) Login(ctx context.Context, request LoginRequest) (LoginResult, error) {
	if f.LoginFunc == nil {
		return LoginResult{}, ErrAuthServiceUnavailable
	}
	return f.LoginFunc(ctx, request)
}

func (f AuthServiceFunc) Refresh(ctx context.Context, request RefreshRequest) (credentials.TokenSet, error) {
	if f.RefreshFunc == nil {
		return credentials.TokenSet{}, ErrAuthServiceUnavailable
	}
	return f.RefreshFunc(ctx, request)
}

// UnavailableAuthService is the safe default until the embedded runtime is
// attached.  It is preferable to a shell-out fallback because an absent
// runtime must be explicit rather than silently using a second Codex install.
type UnavailableAuthService struct{}

func (UnavailableAuthService) Login(context.Context, LoginRequest) (LoginResult, error) {
	return LoginResult{}, ErrAuthServiceUnavailable
}

func (UnavailableAuthService) Refresh(context.Context, RefreshRequest) (credentials.TokenSet, error) {
	return credentials.TokenSet{}, ErrAuthServiceUnavailable
}

// ProfileStatus is a secret-free operator view of one registered profile.
// SavedAt is the vault file modification time, not a claim that the provider
// tokens are currently valid.
type ProfileStatus struct {
	AccountID         string           `json:"account_id"`
	Alias             string           `json:"alias,omitempty"`
	CredentialRef     string           `json:"credential_ref,omitempty"`
	Active            bool             `json:"active"`
	CredentialPresent bool             `json:"credential_present"`
	CredentialHealth  CredentialHealth `json:"credential_health"`
	SavedAt           *time.Time       `json:"saved_at,omitempty"`
	LastTelemetryAt   *time.Time       `json:"last_telemetry_at,omitempty"`
	TelemetrySource   string           `json:"telemetry_source,omitempty"`
}

// LoginOutcome reports non-secret evidence of a completed profile save.
type LoginOutcome struct {
	AccountID string `json:"account_id"`
	Alias     string `json:"alias,omitempty"`
	Activated bool   `json:"activated"`
}

// ActivationOutcome reports a completed local profile selection. Runtime
// AuthManager reload and transport invalidation remain a later transition
// operation owned by the runtime coordinator.
type ActivationOutcome struct {
	AccountID string `json:"account_id"`
}

// RefreshOutcome reports successful token write-back without exposing the
// returned token set.
type RefreshOutcome struct {
	AccountID   string    `json:"account_id"`
	RefreshedAt time.Time `json:"refreshed_at"`
	Activated   bool      `json:"activated"`
}

// ManagerConfig wires durable account state to a native runtime auth service.
// Registry and Vault are required. Deployer/AuthPath are needed for login
// activation, explicit activation, and refresh of the active profile.
type ManagerConfig struct {
	Registry    *FileRegistry
	Vault       credentials.Vault
	Deployer    credentials.CredentialDeployer
	AuthPath    string
	AuthService AuthService
	Now         func() time.Time
}

// Manager owns serialized profile lifecycle mutations.  It does not own the
// Codex runtime; it only supplies a narrow native-auth service seam and an
// atomic deployment boundary for the runtime's auth.json.
type Manager struct {
	registry *FileRegistry
	vault    credentials.Vault
	deployer credentials.CredentialDeployer
	authPath string
	auth     AuthService
	now      func() time.Time

	mu sync.Mutex
}

// NewManager constructs an account manager without reading or writing any
// state.  The default auth service is intentionally unavailable until the
// embedded runtime is attached with SetAuthService.
func NewManager(config ManagerConfig) *Manager {
	auth := config.AuthService
	if auth == nil {
		auth = UnavailableAuthService{}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	deployer := config.Deployer
	if deployer == nil && config.Vault != nil && config.AuthPath != "" {
		deployer = credentials.NewAtomicDeployer(config.Vault, config.AuthPath)
	}
	return &Manager{
		registry: config.Registry,
		vault:    config.Vault,
		deployer: deployer,
		authPath: config.AuthPath,
		auth:     auth,
		now:      now,
	}
}

// SetAuthService attaches the native integrated Codex login/refresh service.
// Passing nil restores the explicit unavailable state; no shell fallback is
// ever installed.
func (m *Manager) SetAuthService(service AuthService) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if service == nil {
		m.auth = UnavailableAuthService{}
		return
	}
	m.auth = service
}

// AuthService reports whether a native service has been attached without
// exposing implementation details. It is intended for diagnostics only.
func (m *Manager) AuthService() AuthService {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auth
}

// Login runs one native login flow and stores the resulting opaque snapshot.
// The first configured account is activated automatically; later accounts
// remain available for selection unless Activate is requested.  Every
// filesystem/registry mutation has a rollback path so a failed activation
// leaves the prior active account deployed and selected.
func (m *Manager) Login(ctx context.Context, request LoginRequest) (LoginOutcome, error) {
	if err := contextError(ctx); err != nil {
		return LoginOutcome{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(true); err != nil {
		return LoginOutcome{}, err
	}
	if err := ValidateAccountIDOptional(request.AccountID); err != nil {
		return LoginOutcome{}, err
	}
	if err := ValidateAlias(request.Alias); err != nil {
		return LoginOutcome{}, err
	}
	auth := m.auth
	result, err := auth.Login(nonNilContext(ctx), request)
	if err != nil {
		return LoginOutcome{}, nativeAuthError(ErrNativeLoginFailed, err)
	}
	if err := contextError(ctx); err != nil {
		return LoginOutcome{}, err
	}

	raw := bytes.Clone(result.AuthJSON)
	if len(raw) == 0 {
		return LoginOutcome{}, fmt.Errorf("%w: native login returned an empty auth snapshot", credentials.ErrInvalidCredential)
	}
	identity, err := credentials.ExtractAccountID(raw)
	if err != nil {
		return LoginOutcome{}, err
	}
	accountID := result.AccountID
	if accountID == "" {
		accountID = identity
	}
	if accountID == "" {
		accountID = request.AccountID
	}
	if accountID == "" {
		return LoginOutcome{}, ErrLoginMissingIdentity
	}
	if err := ValidateAccountID(accountID); err != nil {
		return LoginOutcome{}, err
	}
	if request.AccountID != "" && request.AccountID != accountID {
		return LoginOutcome{}, fmt.Errorf("%w: requested %q, native login returned %q", ErrLoginIdentityMismatch, request.AccountID, accountID)
	}
	if identity != "" && identity != accountID {
		return LoginOutcome{}, fmt.Errorf("%w: snapshot is for %q, result is %q", ErrLoginIdentityMismatch, identity, accountID)
	}

	alias := request.Alias
	if alias == "" {
		alias = result.Alias
	}
	if alias == "" {
		alias = accountID
	}
	if err := ValidateAlias(alias); err != nil {
		return LoginOutcome{}, err
	}
	metadata := cloneMetadata(request.Metadata)
	if metadata == nil && len(result.Metadata) > 0 {
		metadata = make(map[string]string, len(result.Metadata))
	}
	for key, value := range result.Metadata {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	account := Account{
		ID:               accountID,
		Alias:            alias,
		CredentialRef:    accountID,
		Metadata:         metadata,
		CredentialHealth: CredentialHealthHealthy,
	}
	if err := ValidateAccount(account); err != nil {
		return LoginOutcome{}, err
	}

	previous, existed, err := m.registry.Lookup(accountID)
	if err != nil {
		return LoginOutcome{}, err
	}
	previousRaw, hadPreviousRaw, err := loadOptional(m.vault, accountID)
	if err != nil {
		return LoginOutcome{}, err
	}
	if existed && !request.Overwrite {
		return LoginOutcome{}, fmt.Errorf("%w: %s", ErrAccountExists, accountID)
	}
	if !existed && hadPreviousRaw {
		return LoginOutcome{}, fmt.Errorf("%w: orphaned credential snapshot for %q", ErrCredentialStateMismatch, accountID)
	}

	if err := m.vault.Save(accountID, raw); err != nil {
		return LoginOutcome{}, err
	}
	rollback := func(cause error) (LoginOutcome, error) {
		return LoginOutcome{}, errors.Join(cause, m.restoreProfileLocked(accountID, existed, previous, previousRaw, hadPreviousRaw))
	}
	if err := m.registry.Upsert(account); err != nil {
		return rollback(err)
	}

	activeID, err := m.registry.ActiveID()
	if err != nil {
		return rollback(err)
	}
	// An overwrite of the currently selected profile must redeploy the newly
	// authenticated snapshot as well; otherwise the vault and auth.json would
	// silently diverge until a later manual activation.
	activate := request.Activate || activeID == "" || activeID == accountID
	if !activate {
		return LoginOutcome{AccountID: accountID, Alias: alias}, nil
	}
	if _, err := m.activateLocked(ctx, accountID); err != nil {
		return rollback(err)
	}
	return LoginOutcome{AccountID: accountID, Alias: alias, Activated: true}, nil
}

// Add is the profile-oriented spelling for Login. It keeps the public API
// compatible with codex-switch's add/import terminology while still routing
// through the native integrated login service.
func (m *Manager) Add(ctx context.Context, request LoginRequest) (LoginOutcome, error) {
	return m.Login(ctx, request)
}

// Activate selects and atomically deploys a stored profile.  Registry state
// is updated only after the auth file replacement succeeds; any cancellation
// or failure restores the previous file and active marker.
func (m *Manager) Activate(ctx context.Context, accountID string) (ActivationOutcome, error) {
	if err := contextError(ctx); err != nil {
		return ActivationOutcome{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(false); err != nil {
		return ActivationOutcome{}, err
	}
	if err := ValidateAccountID(accountID); err != nil {
		return ActivationOutcome{}, err
	}
	if _, err := m.registry.Get(accountID); err != nil {
		return ActivationOutcome{}, err
	}
	if _, err := m.vault.Load(accountID); err != nil {
		return ActivationOutcome{}, err
	}
	if _, err := m.activateLocked(ctx, accountID); err != nil {
		return ActivationOutcome{}, err
	}
	return ActivationOutcome{AccountID: accountID}, nil
}

// Use is the profile-oriented spelling for Activate.
func (m *Manager) Use(ctx context.Context, accountID string) (ActivationOutcome, error) {
	return m.Activate(ctx, accountID)
}

// Rename changes only the display alias. Stable IDs and credential filenames
// never change, matching codex-switch's profile semantics.
func (m *Manager) Rename(accountID, alias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(false); err != nil {
		return err
	}
	return m.registry.Rename(accountID, alias)
}

// Remove safely removes a non-active profile. An active profile requires an
// explicit force request; force clears the controller marker but deliberately
// leaves the currently deployed auth.json intact so Codex is not logged out
// as a side effect of deleting local profile metadata.
func (m *Manager) Remove(accountID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(false); err != nil {
		return err
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	if _, err := m.registry.Get(accountID); err != nil {
		return err
	}
	activeID, err := m.registry.ActiveID()
	if err != nil {
		return err
	}
	if activeID == accountID && !force {
		return fmt.Errorf("%w: %s", ErrActiveAccount, accountID)
	}
	raw, err := m.vault.Load(accountID)
	if err != nil {
		return err
	}
	if err := m.vault.Delete(accountID); err != nil {
		return err
	}
	if err := m.registry.Remove(accountID, force); err != nil {
		return errors.Join(err, m.vault.Save(accountID, raw))
	}
	return nil
}

// Delete is the explicit destructive spelling for Remove.
func (m *Manager) Delete(accountID string, force bool) error {
	return m.Remove(accountID, force)
}

// Refresh uses the integrated runtime's native refresh implementation and
// persists only non-empty returned token fields. Unknown auth fields are
// retained, and active auth.json is atomically replaced after the vault write.
func (m *Manager) Refresh(ctx context.Context, accountID string) (RefreshOutcome, error) {
	if err := contextError(ctx); err != nil {
		return RefreshOutcome{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(true); err != nil {
		return RefreshOutcome{}, err
	}
	if err := ValidateAccountID(accountID); err != nil {
		return RefreshOutcome{}, err
	}
	if _, err := m.registry.Get(accountID); err != nil {
		return RefreshOutcome{}, err
	}
	raw, err := m.vault.Load(accountID)
	if err != nil {
		return RefreshOutcome{}, err
	}
	auth := m.auth
	tokens, err := auth.Refresh(nonNilContext(ctx), RefreshRequest{AccountID: accountID, AuthJSON: bytes.Clone(raw)})
	if err != nil {
		return RefreshOutcome{}, nativeAuthError(ErrNativeRefreshFailed, err)
	}
	if err := contextError(ctx); err != nil {
		return RefreshOutcome{}, err
	}
	refreshedAt := m.nowUTC()
	updated, err := credentials.MergeTokenWriteBackForAccount(accountID, raw, tokens, refreshedAt)
	if err != nil {
		return RefreshOutcome{}, err
	}
	activeID, err := m.registry.ActiveID()
	if err != nil {
		return RefreshOutcome{}, err
	}
	active := activeID == accountID
	if err := m.vault.Save(accountID, updated); err != nil {
		return RefreshOutcome{}, err
	}
	if active {
		if err := contextError(ctx); err != nil {
			return RefreshOutcome{}, errors.Join(err, m.vault.Save(accountID, raw))
		}
		if m.deployer == nil {
			return RefreshOutcome{}, errors.Join(errors.New("active profile deployer is unavailable"), m.vault.Save(accountID, raw))
		}
		if _, err := m.deployer.Deploy(accountID); err != nil {
			return RefreshOutcome{}, errors.Join(err, m.vault.Save(accountID, raw))
		}
	}
	if err := m.registry.SetCredentialHealth(accountID, CredentialHealthHealthy); err != nil {
		return RefreshOutcome{}, err
	}
	return RefreshOutcome{AccountID: accountID, RefreshedAt: refreshedAt, Activated: active}, nil
}

// Status returns one secret-free profile view.
func (m *Manager) Status(accountID string) (ProfileStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(false); err != nil {
		return ProfileStatus{}, err
	}
	account, err := m.registry.Get(accountID)
	if err != nil {
		return ProfileStatus{}, err
	}
	activeID, err := m.registry.ActiveID()
	if err != nil {
		return ProfileStatus{}, err
	}
	return m.statusLocked(account, activeID == account.ID)
}

// List returns all registered profiles in deterministic ID order.
func (m *Manager) List() ([]ProfileStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateLocked(false); err != nil {
		return nil, err
	}
	accounts, err := m.registry.List()
	if err != nil {
		return nil, err
	}
	activeID, err := m.registry.ActiveID()
	if err != nil {
		return nil, err
	}
	statuses := make([]ProfileStatus, 0, len(accounts))
	for _, account := range accounts {
		status, err := m.statusLocked(account, account.ID == activeID)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (m *Manager) statusLocked(account Account, active bool) (ProfileStatus, error) {
	status := ProfileStatus{
		AccountID:        account.ID,
		Alias:            account.Alias,
		CredentialRef:    account.CredentialRef,
		Active:           active,
		CredentialHealth: account.CredentialHealth,
		TelemetrySource:  account.TelemetrySource,
	}
	if account.LastTelemetryAt != nil {
		at := account.LastTelemetryAt.UTC()
		status.LastTelemetryAt = &at
	}
	if snapshotReader, ok := m.vault.(interface {
		Read(string) (credentials.Snapshot, error)
	}); ok {
		snapshot, err := snapshotReader.Read(account.ID)
		if err == nil {
			status.CredentialPresent = true
			saved := snapshot.SavedAt.UTC()
			status.SavedAt = &saved
			return status, nil
		}
		if !errors.Is(err, credentials.ErrCredentialNotFound) {
			return ProfileStatus{}, err
		}
		return status, nil
	}
	if _, err := m.vault.Load(account.ID); err == nil {
		status.CredentialPresent = true
	} else if !errors.Is(err, credentials.ErrCredentialNotFound) {
		return ProfileStatus{}, err
	}
	return status, nil
}

func (m *Manager) activateLocked(ctx context.Context, accountID string) (ActivationOutcome, error) {
	if m.deployer == nil {
		return ActivationOutcome{}, errors.New("active profile deployer is unavailable")
	}
	if err := contextError(ctx); err != nil {
		return ActivationOutcome{}, err
	}
	previousID, err := m.registry.ActiveID()
	if err != nil {
		return ActivationOutcome{}, err
	}
	previousAuth, hadAuth, err := readOptionalFile(m.authPath)
	if err != nil {
		return ActivationOutcome{}, err
	}
	if _, err := m.deployer.Deploy(accountID); err != nil {
		return ActivationOutcome{}, err
	}
	rollback := func(cause error) (ActivationOutcome, error) {
		return ActivationOutcome{}, errors.Join(cause, m.restoreActivationLocked(previousID, previousAuth, hadAuth))
	}
	if err := contextError(ctx); err != nil {
		return rollback(err)
	}
	if err := m.registry.SetActive(accountID); err != nil {
		return rollback(err)
	}
	return ActivationOutcome{AccountID: accountID}, nil
}

func (m *Manager) restoreActivationLocked(previousID string, previousAuth []byte, hadAuth bool) error {
	var restoreErr error
	if m.authPath == "" {
		restoreErr = errors.New("auth path is empty; cannot restore previous deployment")
	} else if hadAuth {
		restoreErr = credentials.WriteAtomically(m.authPath, previousAuth)
	} else if err := os.Remove(m.authPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		restoreErr = err
	}
	var markerErr error
	if previousID == "" {
		markerErr = m.registry.ClearActive()
	} else {
		markerErr = m.registry.SetActive(previousID)
	}
	return errors.Join(restoreErr, markerErr)
}

func (m *Manager) restoreProfileLocked(accountID string, existed bool, previous Account, previousRaw []byte, hadPreviousRaw bool) error {
	var vaultErr error
	if hadPreviousRaw {
		vaultErr = m.vault.Save(accountID, previousRaw)
	} else {
		if err := m.vault.Delete(accountID); err != nil && !errors.Is(err, credentials.ErrCredentialNotFound) {
			vaultErr = err
		}
	}
	var registryErr error
	if existed {
		registryErr = m.registry.Upsert(previous)
	} else {
		if err := m.registry.Remove(accountID, true); err != nil && !errors.Is(err, ErrAccountNotFound) {
			registryErr = err
		}
	}
	return errors.Join(vaultErr, registryErr)
}

func (m *Manager) validateLocked(requireAuth bool) error {
	if m == nil {
		return errors.New("nil account manager")
	}
	if m.registry == nil {
		return errors.New("account manager registry is nil")
	}
	if m.vault == nil {
		return errors.New("account manager vault is nil")
	}
	if requireAuth && m.auth == nil {
		return ErrAuthServiceUnavailable
	}
	return nil
}

func (m *Manager) nowUTC() time.Time {
	now := m.now
	if now == nil {
		now = time.Now
	}
	return now().UTC()
}

func ValidateAccountIDOptional(accountID string) error {
	if accountID == "" {
		return nil
	}
	return ValidateAccountID(accountID)
}

func loadOptional(vault credentials.Vault, accountID string) ([]byte, bool, error) {
	raw, err := vault.Load(accountID)
	if err == nil {
		return raw, true, nil
	}
	if errors.Is(err, credentials.ErrCredentialNotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

func readOptionalFile(path string) ([]byte, bool, error) {
	if path == "" {
		return nil, false, errors.New("auth path is empty")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		return bytes.Clone(raw), true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func nativeAuthError(kind, err error) error {
	if err == nil {
		return kind
	}
	if errors.Is(err, ErrAuthServiceUnavailable) {
		return ErrAuthServiceUnavailable
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Do not wrap the provider error: callers print returned errors, and a
	// provider-controlled message could contain access/refresh token material.
	return kind
}
