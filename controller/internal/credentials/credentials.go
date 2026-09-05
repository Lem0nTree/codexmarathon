// Package credentials owns the controller's credential snapshots and the
// boundary at which a snapshot is deployed to Codex's auth.json.
//
// The runtime remains the authority for parsing and loading auth.json. The
// controller stores opaque, validated JSON snapshots and never needs to
// retain a second in-memory authentication object.
package credentials

import (
	"bytes"
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

var (
	// ErrInvalidAccountID is returned when an account identifier cannot be used
	// as a single vault filename component.
	ErrInvalidAccountID = errors.New("invalid account id")
	// ErrCredentialNotFound indicates that no snapshot exists for an account.
	ErrCredentialNotFound = errors.New("credential snapshot not found")
	// ErrInvalidCredential indicates malformed or non-object auth JSON.
	ErrInvalidCredential = errors.New("invalid credential snapshot")
	// ErrCredentialAccountMismatch prevents a snapshot for one account from
	// being written under another account's key.
	ErrCredentialAccountMismatch = errors.New("credential account mismatch")
	// ErrEmptyTokenUpdate indicates that a write-back request contained no
	// token material or account identity.
	ErrEmptyTokenUpdate = errors.New("empty token write-back")
)

// TokenSet is the small token contract shared by the controller vault and the
// runtime adapter. Values are write-only from the perspective of the journal;
// they are never included in journal.Event.
type TokenSet struct {
	AccessToken  string `json:"access_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// Tokens is retained as a readable alias for integrations that use the donor
// project's terminology.
type Tokens = TokenSet

// TokenWriteBackRequest describes a runtime refresh that should be persisted
// to the account's stored snapshot. RefreshedAt is optional; a zero value is
// replaced by the vault clock.
type TokenWriteBackRequest struct {
	AccountID   string
	Tokens      TokenSet
	RefreshedAt time.Time
}

// Snapshot is a defensive view of one stored credential file. Raw is copied
// on ingress and egress so callers cannot mutate vault-owned data.
type Snapshot struct {
	AccountID string
	Raw       []byte
	SavedAt   time.Time
}

// Vault is the controller abstraction used by transition code. The runtime
// consumes a deployed auth.json; it does not access this vault directly.
type Vault interface {
	Save(accountID string, raw []byte) error
	Load(accountID string) ([]byte, error)
	Delete(accountID string) error
	List() ([]string, error)
	WriteBack(request TokenWriteBackRequest) error
}

// CredentialVault is a descriptive alias for Vault.
type CredentialVault = Vault

// TokenWriteBackSink is the explicit runtime-refresh persistence seam. A
// runtime adapter may depend on this interface without depending on the file
// implementation.
type TokenWriteBackSink interface {
	WriteBack(request TokenWriteBackRequest) error
}

// SnapshotReader is the narrow source needed by AtomicDeployer. Keeping this
// interface small allows transition tests to use an in-memory fake without
// coupling them to the file-backed vault.
type SnapshotReader interface {
	Load(accountID string) ([]byte, error)
}

// SnapshotWriter is the narrow sink used when a live runtime synchronizes
// the active account's refreshed opaque auth snapshot before a switch. It is
// intentionally separate from Vault so transition code cannot enumerate or
// delete credentials as part of that synchronization step.
type SnapshotWriter interface {
	Save(accountID string, raw []byte) error
}

// FileVault is the MVP credential vault. Each account is stored as an opaque
// JSON document in <dir>/<accountID>.json. The directory is created with
// owner-only permissions and files are written with owner read/write
// permissions.
type FileVault struct {
	dir string
	mu  sync.RWMutex
	now func() time.Time
}

// NewFileVault constructs a file-backed vault rooted at dir. Construction is
// intentionally side-effect free; the directory is created by the first
// mutating operation.
func NewFileVault(dir string) *FileVault {
	return &FileVault{dir: dir, now: time.Now}
}

// NewVault is the short constructor used by controller wiring.
func NewVault(dir string) *FileVault {
	return NewFileVault(dir)
}

// Dir returns the vault root. It is useful for diagnostics and tests and does
// not expose any credential contents.
func (v *FileVault) Dir() string {
	if v == nil {
		return ""
	}
	return v.dir
}

// SetClock injects a clock for deterministic tests. A nil clock restores the
// wall clock.
func (v *FileVault) SetClock(now func() time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if now == nil {
		v.now = time.Now
		return
	}
	v.now = now
}

// Save validates and atomically stores an account snapshot.
func (v *FileVault) Save(accountID string, raw []byte) error {
	if v == nil {
		return errors.New("nil credential vault")
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	canonical, err := validateSnapshotForAccount(accountID, raw)
	if err != nil {
		return err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureDir(); err != nil {
		return err
	}
	return WriteAtomically(v.pathUnchecked(accountID), canonical)
}

// Put is a compatibility spelling for Save.
func (v *FileVault) Put(accountID string, raw []byte) error {
	return v.Save(accountID, raw)
}

// Load returns a defensive copy of the raw JSON snapshot.
func (v *FileVault) Load(accountID string) ([]byte, error) {
	if v == nil {
		return nil, errors.New("nil credential vault")
	}
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	path := v.pathUnchecked(accountID)
	if err := rejectCredentialSymlink(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrCredentialNotFound, accountID)
		}
		return nil, err
	}
	canonical, err := validateSnapshotForAccount(accountID, raw)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(canonical), nil
}

// Read returns a snapshot with file metadata. It is convenient when registry
// code needs to expose a saved-at timestamp without reopening the file.
func (v *FileVault) Read(accountID string) (Snapshot, error) {
	if v == nil {
		return Snapshot{}, errors.New("nil credential vault")
	}
	if err := validateAccountID(accountID); err != nil {
		return Snapshot{}, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	path := v.pathUnchecked(accountID)
	if err := rejectCredentialSymlink(path); err != nil {
		return Snapshot{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrCredentialNotFound, accountID)
		}
		return Snapshot{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	canonical, err := validateSnapshotForAccount(accountID, raw)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{AccountID: accountID, Raw: bytes.Clone(canonical), SavedAt: info.ModTime().UTC()}, nil
}

// Get is an alias for Read.
func (v *FileVault) Get(accountID string) (Snapshot, error) {
	return v.Read(accountID)
}

// List returns account identifiers in stable lexical order. Non-JSON files
// are ignored so an operator can keep a README or backup outside the vault
// contract without changing account discovery.
func (v *FileVault) List() ([]string, error) {
	if v == nil {
		return nil, errors.New("nil credential vault")
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	entries, err := os.ReadDir(v.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	accounts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		if validateAccountID(name) == nil {
			accounts = append(accounts, name)
		}
	}
	sort.Strings(accounts)
	return accounts, nil
}

// Delete removes a stored snapshot.
func (v *FileVault) Delete(accountID string) error {
	if v == nil {
		return errors.New("nil credential vault")
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	path := v.pathUnchecked(accountID)
	if err := rejectCredentialSymlink(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrCredentialNotFound, accountID)
		}
		return err
	}
	return nil
}

// Remove is an alias for Delete.
func (v *FileVault) Remove(accountID string) error {
	return v.Delete(accountID)
}

// WriteBack merges refreshed runtime tokens into the existing snapshot and
// atomically replaces that snapshot. Unknown auth fields are retained.
func (v *FileVault) WriteBack(request TokenWriteBackRequest) error {
	if v == nil {
		return errors.New("nil credential vault")
	}
	if err := validateAccountID(request.AccountID); err != nil {
		return err
	}
	if request.Tokens.AccountID != "" && request.Tokens.AccountID != request.AccountID {
		return fmt.Errorf("%w: write-back is for %q, requested %q", ErrCredentialAccountMismatch, request.Tokens.AccountID, request.AccountID)
	}
	refreshedAt := request.RefreshedAt
	if refreshedAt.IsZero() {
		v.mu.RLock()
		now := v.now
		v.mu.RUnlock()
		if now == nil {
			now = time.Now
		}
		refreshedAt = now()
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	path := v.pathUnchecked(request.AccountID)
	if err := rejectCredentialSymlink(path); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrCredentialNotFound, request.AccountID)
		}
		return err
	}
	updated, err := MergeTokenWriteBackForAccount(request.AccountID, raw, request.Tokens, refreshedAt)
	if err != nil {
		return err
	}
	return WriteAtomically(path, updated)
}

// WriteBackTokens is a convenience method for callers that already have the
// account identifier separated from the runtime token payload.
func (v *FileVault) WriteBackTokens(accountID string, tokens TokenSet, refreshedAt time.Time) error {
	return v.WriteBack(TokenWriteBackRequest{AccountID: accountID, Tokens: tokens, RefreshedAt: refreshedAt})
}

// Deploy installs one snapshot into authPath using the vault's atomic
// deployment boundary. It is a convenience for small controllers; larger
// transition coordinators can construct an AtomicDeployer directly and retain
// an explicit deployment result.
func (v *FileVault) Deploy(accountID, authPath string) (DeploymentResult, error) {
	return NewAtomicDeployer(v, authPath).Deploy(accountID)
}

func (v *FileVault) ensureDir() error {
	if v.dir == "" {
		return errors.New("credential vault directory is empty")
	}
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	// Tighten an existing directory created with broad permissions. On
	// Windows this is best effort because ACLs, not mode bits, are authoritative.
	if err := os.Chmod(v.dir, 0o700); err != nil && !isPermissionMetadataError(err) {
		return err
	}
	return nil
}

func (v *FileVault) pathUnchecked(accountID string) string {
	return filepath.Join(v.dir, accountID+".json")
}

func validateAccountID(accountID string) error {
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

// ValidateSnapshot verifies that raw is a JSON object and returns a defensive
// copy of the opaque document. It deliberately does not interpret
// provider-specific auth fields; Codext remains responsible for that schema.
func ValidateSnapshot(raw []byte) ([]byte, error) {
	return validateSnapshotForAccount("", raw)
}

func validateSnapshotForAccount(accountID string, raw []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%w: expected JSON object", ErrInvalidCredential)
	}
	if accountID != "" {
		identity, err := accountIDFromObject(object)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
		}
		if identity != "" && identity != accountID {
			return nil, fmt.Errorf("%w: snapshot is for %q, requested %q", ErrCredentialAccountMismatch, identity, accountID)
		}
	}
	// Keep the original bytes for snapshots. Auth formats can contain fields
	// unknown to this controller, and preserving the opaque document avoids an
	// unnecessary reformat before Codext reads it.
	return bytes.Clone(raw), nil
}

func accountIDFromObject(object map[string]json.RawMessage) (string, error) {
	var topLevel string
	if value, ok := object["account_id"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &topLevel); err != nil {
			return "", err
		}
	}
	value, ok := object["tokens"]
	if !ok || string(value) == "null" {
		return topLevel, nil
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(value, &tokens); err != nil {
		return "", err
	}
	if tokens == nil {
		return "", nil
	}
	var nested string
	if value, ok := tokens["account_id"]; ok && string(value) != "null" {
		if err := json.Unmarshal(value, &nested); err != nil {
			return "", err
		}
	}
	if topLevel != "" && nested != "" && topLevel != nested {
		return "", fmt.Errorf("conflicting account_id values %q and %q", topLevel, nested)
	}
	if topLevel != "" {
		return topLevel, nil
	}
	return nested, nil
}

// ExtractAccountID reads the optional account identity without exposing any
// token values. An empty result means the auth format did not include identity.
func ExtractAccountID(raw []byte) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("expected JSON object")
		}
		return "", fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	identity, err := accountIDFromObject(object)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	return identity, nil
}

// MergeTokenWriteBack preserves all unknown fields in an auth snapshot while
// merging non-empty refreshed token fields and a last_refresh timestamp.
func MergeTokenWriteBack(raw []byte, tokens TokenSet, refreshedAt time.Time) ([]byte, error) {
	return MergeTokenWriteBackForAccount("", raw, tokens, refreshedAt)
}

// MergeTokenWriteBackForAccount additionally ensures that an identity present
// in the existing document matches accountID.
func MergeTokenWriteBackForAccount(accountID string, raw []byte, tokens TokenSet, refreshedAt time.Time) ([]byte, error) {
	if tokens.AccessToken == "" && tokens.IDToken == "" && tokens.RefreshToken == "" && tokens.AccountID == "" {
		return nil, ErrEmptyTokenUpdate
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("expected JSON object")
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if accountID != "" {
		identity, err := accountIDFromObject(object)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
		}
		if identity != "" && identity != accountID {
			return nil, fmt.Errorf("%w: snapshot is for %q, requested %q", ErrCredentialAccountMismatch, identity, accountID)
		}
		if tokens.AccountID != "" && tokens.AccountID != accountID {
			return nil, fmt.Errorf("%w: write-back is for %q, requested %q", ErrCredentialAccountMismatch, tokens.AccountID, accountID)
		}
	}

	merged := map[string]json.RawMessage{}
	if existing, ok := object["tokens"]; ok && len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &merged); err != nil {
			return nil, fmt.Errorf("%w: malformed tokens object: %v", ErrInvalidCredential, err)
		}
		if merged == nil {
			merged = map[string]json.RawMessage{}
		}
	}
	setToken := func(name, value string) {
		if value == "" {
			return
		}
		encoded, _ := json.Marshal(value)
		merged[name] = encoded
	}
	setToken("access_token", tokens.AccessToken)
	setToken("id_token", tokens.IDToken)
	setToken("refresh_token", tokens.RefreshToken)
	setToken("account_id", tokens.AccountID)
	encodedTokens, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	object["tokens"] = encodedTokens
	if !refreshedAt.IsZero() {
		encodedRefresh, err := json.Marshal(refreshedAt.UTC())
		if err != nil {
			return nil, err
		}
		object["last_refresh"] = encodedRefresh
	}
	updated, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	return updated, nil
}

func rejectCredentialSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: credential path is a symbolic link", ErrInvalidCredential)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: credential path is a directory", ErrInvalidCredential)
	}
	return nil
}

// UpdateTokens is the donor-compatible spelling for the controller's
// token-write-back merge contract.
func UpdateTokens(raw []byte, tokens TokenSet, refreshedAt time.Time) ([]byte, error) {
	return MergeTokenWriteBack(raw, tokens, refreshedAt)
}
