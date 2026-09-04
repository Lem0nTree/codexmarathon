// Package accounts owns the durable controller-side account registry.
//
// An Account is identity and metadata only. Credential material belongs to
// credentials.FileVault and is referenced by CredentialRef; keeping these
// domains separate prevents registry reads and journal entries from becoming
// accidental token stores.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const registryVersion = 1

var (
	ErrInvalidAccountID = errors.New("invalid account id")
	ErrAccountNotFound  = errors.New("account not found")
	ErrAccountExists    = errors.New("account already exists")
	ErrActiveAccount    = errors.New("account is active")
	ErrInvalidRegistry  = errors.New("invalid account registry")
)

// CredentialHealth records the controller's knowledge about the referenced
// snapshot. It is deliberately not a credential validity claim made by the
// runtime; it is a small operator-facing state marker.
type CredentialHealth string

const (
	CredentialHealthUnknown  CredentialHealth = "unknown"
	CredentialHealthHealthy  CredentialHealth = "healthy"
	CredentialHealthStale    CredentialHealth = "stale"
	CredentialHealthInvalid  CredentialHealth = "invalid"
)

// Account is the non-secret registry record for one configured identity.
// Metadata values must be descriptive only; raw auth JSON and token fields are
// intentionally not representable here.
type Account struct {
	ID                string            `json:"id"`
	Alias             string            `json:"alias,omitempty"`
	CredentialRef     string            `json:"credential_ref,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	LastTelemetryAt   *time.Time        `json:"last_telemetry_at,omitempty"`
	TelemetrySource   string            `json:"telemetry_source,omitempty"`
	CredentialHealth  CredentialHealth  `json:"credential_health,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// RegistryState is the on-disk representation. The map is private to
// FileRegistry operations; callers receive sorted copies instead.
type RegistryState struct {
	Version         int                `json:"version"`
	ActiveAccountID string             `json:"active_account_id,omitempty"`
	Accounts        map[string]Account `json:"accounts"`
}

// FileRegistry is a JSON-backed account registry. Each operation reloads the
// file while holding the process mutex, so multiple FileRegistry instances in
// one process do not operate on stale state. Cross-process writers are outside
// this MVP; atomic replacement keeps readers from observing partial JSON.
type FileRegistry struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewFileRegistry constructs a registry at path without creating it.
func NewFileRegistry(path string) *FileRegistry {
	return &FileRegistry{path: path, now: time.Now}
}

// NewRegistry is the concise constructor used by controller wiring.
func NewRegistry(path string) *FileRegistry {
	return NewFileRegistry(path)
}

// Path returns the registry file path and does not expose account contents.
func (r *FileRegistry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// SetClock injects a clock for deterministic tests.
func (r *FileRegistry) SetClock(now func() time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if now == nil {
		r.now = time.Now
		return
	}
	r.now = now
}

// Register creates a new account and rejects duplicate IDs.
func (r *FileRegistry) Register(account Account) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccount(account); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	if _, exists := state.Accounts[account.ID]; exists {
		return fmt.Errorf("%w: %s", ErrAccountExists, account.ID)
	}
	now := r.clockNow()
	account = normalizeAccount(account, now, false)
	state.Accounts[account.ID] = account
	return r.saveUnlocked(state)
}

// Upsert creates or updates an account. Existing CreatedAt and omitted
// CredentialRef/health values are preserved on update so a metadata refresh
// cannot accidentally unlink credentials.
func (r *FileRegistry) Upsert(account Account) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccount(account); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	existing, exists := state.Accounts[account.ID]
	if exists {
		if account.CredentialRef == "" {
			account.CredentialRef = existing.CredentialRef
		}
		if account.CredentialHealth == "" {
			account.CredentialHealth = existing.CredentialHealth
		}
		account.CreatedAt = existing.CreatedAt
		if account.LastTelemetryAt == nil {
			account.LastTelemetryAt = existing.LastTelemetryAt
		}
		if account.TelemetrySource == "" {
			account.TelemetrySource = existing.TelemetrySource
		}
	}
	account = normalizeAccount(account, r.clockNow(), exists)
	state.Accounts[account.ID] = account
	return r.saveUnlocked(state)
}

// Rename updates the operator-facing alias for one account without changing
// its credential reference, telemetry markers, or active selection.  The
// operation is intentionally registry-only: credential files are keyed by the
// stable account ID and therefore never move during a rename.
func (r *FileRegistry) Rename(accountID, alias string) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	account, ok := state.Accounts[accountID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	account.Alias = alias
	account.UpdatedAt = r.clockNow()
	state.Accounts[accountID] = account
	return r.saveUnlocked(state)
}

// Save is an alias for Upsert.
func (r *FileRegistry) Save(account Account) error {
	return r.Upsert(account)
}

// Get returns a defensive account copy.
func (r *FileRegistry) Get(accountID string) (Account, error) {
	if r == nil {
		return Account{}, errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return Account{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return Account{}, err
	}
	account, ok := state.Accounts[accountID]
	if !ok {
		return Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	return cloneAccount(account), nil
}

// Lookup returns a boolean instead of ErrAccountNotFound for callers that
// treat absent optional accounts as normal.
func (r *FileRegistry) Lookup(accountID string) (Account, bool, error) {
	account, err := r.Get(accountID)
	if errors.Is(err, ErrAccountNotFound) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return account, true, nil
}

// List returns accounts in stable ID order.
func (r *FileRegistry) List() ([]Account, error) {
	if r == nil {
		return nil, errors.New("nil account registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(state.Accounts))
	for id := range state.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	accounts := make([]Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, cloneAccount(state.Accounts[id]))
	}
	return accounts, nil
}

// SetActive marks a configured account as the desired active identity.
func (r *FileRegistry) SetActive(accountID string) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	if _, ok := state.Accounts[accountID]; !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if state.ActiveAccountID == accountID {
		return nil
	}
	state.ActiveAccountID = accountID
	return r.saveUnlocked(state)
}

// ClearActive removes the active identity marker.
func (r *FileRegistry) ClearActive() error {
	if r == nil {
		return errors.New("nil account registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	if state.ActiveAccountID == "" {
		return nil
	}
	state.ActiveAccountID = ""
	return r.saveUnlocked(state)
}

// ActiveID returns the desired active account identifier.
func (r *FileRegistry) ActiveID() (string, error) {
	if r == nil {
		return "", errors.New("nil account registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return "", err
	}
	return state.ActiveAccountID, nil
}

// Active returns the active account and false when no account is selected.
func (r *FileRegistry) Active() (Account, bool, error) {
	if r == nil {
		return Account{}, false, errors.New("nil account registry")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return Account{}, false, err
	}
	if state.ActiveAccountID == "" {
		return Account{}, false, nil
	}
	account, ok := state.Accounts[state.ActiveAccountID]
	if !ok {
		return Account{}, false, fmt.Errorf("%w: active account %s", ErrInvalidRegistry, state.ActiveAccountID)
	}
	return cloneAccount(account), true, nil
}

// Remove deletes an account. By default removing the active account is
// rejected; pass force=true only when the caller intentionally clears active
// state as part of the same operation.
func (r *FileRegistry) Remove(accountID string, force ...bool) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	forced := len(force) > 0 && force[0]
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	if _, ok := state.Accounts[accountID]; !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if state.ActiveAccountID == accountID && !forced {
		return fmt.Errorf("%w: %s", ErrActiveAccount, accountID)
	}
	delete(state.Accounts, accountID)
	if state.ActiveAccountID == accountID {
		state.ActiveAccountID = ""
	}
	return r.saveUnlocked(state)
}

// Delete is an explicit spelling for Remove.
func (r *FileRegistry) Delete(accountID string) error {
	return r.Remove(accountID)
}

// SetTelemetry updates only the non-secret telemetry marker for an account.
func (r *FileRegistry) SetTelemetry(accountID string, observedAt time.Time, source string) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	account, ok := state.Accounts[accountID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	observedAt = observedAt.UTC()
	account.LastTelemetryAt = &observedAt
	account.TelemetrySource = strings.TrimSpace(source)
	account.UpdatedAt = r.clockNow()
	state.Accounts[accountID] = account
	return r.saveUnlocked(state)
}

// SetCredentialHealth updates the controller marker for an account without
// touching credential bytes.
func (r *FileRegistry) SetCredentialHealth(accountID string, health CredentialHealth) error {
	if r == nil {
		return errors.New("nil account registry")
	}
	if err := ValidateAccountID(accountID); err != nil {
		return err
	}
	if !validCredentialHealth(health) {
		return fmt.Errorf("%w: unknown credential health %q", ErrInvalidRegistry, health)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.loadUnlocked()
	if err != nil {
		return err
	}
	account, ok := state.Accounts[accountID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	account.CredentialHealth = health
	account.UpdatedAt = r.clockNow()
	state.Accounts[accountID] = account
	return r.saveUnlocked(state)
}

// ValidateAccount validates an account record and rejects fields that could
// make its identity ambiguous or unsafe as a vault reference.
func ValidateAccount(account Account) error {
	if err := ValidateAccountID(account.ID); err != nil {
		return err
	}
	if err := ValidateAlias(account.Alias); err != nil {
		return err
	}
	if account.CredentialRef != "" {
		if err := ValidateAccountID(account.CredentialRef); err != nil {
			return fmt.Errorf("%w: credential ref: %v", ErrInvalidRegistry, err)
		}
	}
	if account.CredentialHealth != "" && !validCredentialHealth(account.CredentialHealth) {
		return fmt.Errorf("%w: unknown credential health %q", ErrInvalidRegistry, account.CredentialHealth)
	}
	for key, value := range account.Metadata {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") || strings.ContainsRune(key, 0) {
			return fmt.Errorf("%w: invalid metadata key", ErrInvalidRegistry)
		}
		if strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: invalid metadata value", ErrInvalidRegistry)
		}
		if looksSensitiveMetadata(key) {
			return fmt.Errorf("%w: sensitive metadata key %q", ErrInvalidRegistry, key)
		}
	}
	return nil
}

// ValidateAlias enforces the non-secret, operator-facing profile label
// contract. Empty aliases are allowed so a provider identity or account ID can
// be used as the display fallback, but a supplied alias must be a single-line
// value without surrounding whitespace or NUL bytes.
func ValidateAlias(alias string) error {
	if strings.TrimSpace(alias) != alias {
		return fmt.Errorf("%w: alias has surrounding whitespace", ErrInvalidRegistry)
	}
	if strings.ContainsRune(alias, 0) || strings.ContainsAny(alias, "\r\n") {
		return fmt.Errorf("%w: invalid alias", ErrInvalidRegistry)
	}
	if len(alias) > 256 {
		return fmt.Errorf("%w: alias is too long", ErrInvalidRegistry)
	}
	return nil
}

// ValidateAccountID ensures an ID is safe to use as a vault filename
// component. Account IDs can be provider IDs or local stable identifiers, so
// this validation intentionally permits spaces and punctuation except path and
// control characters.
func ValidateAccountID(accountID string) error {
	if accountID == "" || accountID == "." || accountID == ".." || len(accountID) > 240 {
		return ErrInvalidAccountID
	}
	if strings.ContainsAny(accountID, `/\\<>:"|?*`) || strings.ContainsRune(accountID, 0) {
		return ErrInvalidAccountID
	}
	if strings.HasSuffix(accountID, ".") || strings.HasSuffix(accountID, " ") {
		return ErrInvalidAccountID
	}
	for _, r := range accountID {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidAccountID
		}
	}
	base := strings.ToUpper(strings.SplitN(accountID, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return ErrInvalidAccountID
	}
	return nil
}

func (r *FileRegistry) loadUnlocked() (RegistryState, error) {
	state := RegistryState{Version: registryVersion, Accounts: map[string]Account{}}
	if r.path == "" {
		return RegistryState{}, errors.New("account registry path is empty")
	}
	if err := rejectRegistrySymlink(r.path); err != nil {
		return RegistryState{}, err
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return RegistryState{}, err
	}
	if len(raw) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return RegistryState{}, fmt.Errorf("%w: decode: %v", ErrInvalidRegistry, err)
	}
	if state.Version == 0 {
		state.Version = registryVersion
	}
	if state.Version != registryVersion {
		return RegistryState{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidRegistry, state.Version)
	}
	if state.Accounts == nil {
		state.Accounts = map[string]Account{}
	}
	for id, account := range state.Accounts {
		if id != account.ID {
			return RegistryState{}, fmt.Errorf("%w: map key %q does not match account id %q", ErrInvalidRegistry, id, account.ID)
		}
		if err := ValidateAccount(account); err != nil {
			return RegistryState{}, fmt.Errorf("%w: account %q: %v", ErrInvalidRegistry, id, err)
		}
	}
	if state.ActiveAccountID != "" {
		if err := ValidateAccountID(state.ActiveAccountID); err != nil {
			return RegistryState{}, fmt.Errorf("%w: active id: %v", ErrInvalidRegistry, err)
		}
		if _, ok := state.Accounts[state.ActiveAccountID]; !ok {
			return RegistryState{}, fmt.Errorf("%w: active account %q is not registered", ErrInvalidRegistry, state.ActiveAccountID)
		}
	}
	return state, nil
}

func (r *FileRegistry) saveUnlocked(state RegistryState) error {
	if r.path == "" {
		return errors.New("account registry path is empty")
	}
	if state.Accounts == nil {
		state.Accounts = map[string]Account{}
	}
	state.Version = registryVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil && !isPermissionMetadataError(err) {
		return err
	}
	if err := rejectRegistrySymlink(r.path); err != nil {
		return err
	}
	return writeRegistryAtomically(r.path, data)
}

func (r *FileRegistry) clockNow() time.Time {
	now := r.now
	if now == nil {
		now = time.Now
	}
	return now().UTC()
}

func normalizeAccount(account Account, now time.Time, existing bool) Account {
	if account.CredentialRef == "" {
		account.CredentialRef = account.ID
	}
	if account.CredentialHealth == "" {
		account.CredentialHealth = CredentialHealthUnknown
	}
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	// UpdatedAt is controller-owned evidence of the write, even when a
	// caller supplies a stale timestamp. The existing flag remains in the
	// signature for readability at call sites and future migration hooks.
	_ = existing
	account.UpdatedAt = now
	account.CreatedAt = account.CreatedAt.UTC()
	account.UpdatedAt = account.UpdatedAt.UTC()
	account.Metadata = cloneMetadata(account.Metadata)
	if account.LastTelemetryAt != nil {
		observedAt := account.LastTelemetryAt.UTC()
		account.LastTelemetryAt = &observedAt
	}
	return account
}

func cloneAccount(account Account) Account {
	account.Metadata = cloneMetadata(account.Metadata)
	if account.LastTelemetryAt != nil {
		observedAt := account.LastTelemetryAt.UTC()
		account.LastTelemetryAt = &observedAt
	}
	return account
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}

func validCredentialHealth(health CredentialHealth) bool {
	switch health {
	case CredentialHealthUnknown, CredentialHealthHealthy, CredentialHealthStale, CredentialHealthInvalid:
		return true
	default:
		return false
	}
}

func isPermissionMetadataError(error) bool {
	return false
}

func looksSensitiveMetadata(key string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, marker := range []string{
		"access_token", "id_token", "refresh_token", "authorization", "api_key",
		"password", "secret", "auth_json", "credential", "private_key",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func rejectRegistrySymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: registry path is a symbolic link", ErrInvalidRegistry)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: registry path is a directory", ErrInvalidRegistry)
	}
	return nil
}
