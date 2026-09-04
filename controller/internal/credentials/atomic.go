package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeploymentResult contains non-secret evidence about a successful atomic
// auth.json replacement.
type DeploymentResult struct {
	AccountID  string
	Path       string
	DeployedAt time.Time
}

// AtomicDeployer reads an opaque snapshot from a SnapshotReader and replaces
// the runtime's auth file at a safe filesystem boundary.
type AtomicDeployer struct {
	Source   SnapshotReader
	AuthPath string
	now      func() time.Time
}

// CredentialDeployer is the narrow deployment seam used by transitions.
type CredentialDeployer interface {
	Deploy(accountID string) (DeploymentResult, error)
}

// AtomicCredentialDeployer is a descriptive alias for AtomicDeployer.
type AtomicCredentialDeployer = AtomicDeployer

// NewAtomicDeployer constructs a deployer. It does not write anything until
// Deploy is called.
func NewAtomicDeployer(source SnapshotReader, authPath string) *AtomicDeployer {
	return &AtomicDeployer{Source: source, AuthPath: authPath, now: time.Now}
}

// SetClock injects a clock for deterministic deployment records.
func (d *AtomicDeployer) SetClock(now func() time.Time) {
	if now == nil {
		d.now = time.Now
		return
	}
	d.now = now
}

// Deploy atomically installs accountID's snapshot into AuthPath. The file is
// written in the destination directory, synced, and then replaced using the
// platform's atomic replace primitive. The temporary file is cleaned up on
// every failure path.
func (d *AtomicDeployer) Deploy(accountID string) (DeploymentResult, error) {
	if d == nil {
		return DeploymentResult{}, errors.New("nil atomic deployer")
	}
	if d.Source == nil {
		return DeploymentResult{}, errors.New("atomic deployer source is nil")
	}
	if err := validateAccountID(accountID); err != nil {
		return DeploymentResult{}, err
	}
	raw, err := d.Source.Load(accountID)
	if err != nil {
		return DeploymentResult{}, err
	}
	if err := d.DeployRaw(accountID, raw); err != nil {
		return DeploymentResult{}, err
	}
	now := d.now
	if now == nil {
		now = time.Now
	}
	return DeploymentResult{AccountID: accountID, Path: d.AuthPath, DeployedAt: now().UTC()}, nil
}

// DeployRaw is useful for a transition that has already read and verified a
// snapshot from a different vault implementation.
func (d *AtomicDeployer) DeployRaw(accountID string, raw []byte) error {
	if d == nil {
		return errors.New("nil atomic deployer")
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	if d.AuthPath == "" {
		return errors.New("auth path is empty")
	}
	canonical, err := validateSnapshotForAccount(accountID, raw)
	if err != nil {
		return err
	}
	return WriteAtomically(d.AuthPath, canonical)
}

// WriteAtomically replaces path with data. The temporary file and final file
// share a directory so the final replacement is a single filesystem rename.
func WriteAtomically(path string, data []byte) error {
	if path == "" {
		return errors.New("destination path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil && !isPermissionMetadataError(err) {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".codexmarathon-auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := replaceFile(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if err := syncDirectory(dir); err != nil {
		// The data and rename are complete. Return the durability error so a
		// caller can record an uncertain deployment rather than claiming that
		// directory metadata was definitely persisted.
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// AtomicWriteFile is a descriptive alias used by controller code.
func AtomicWriteFile(path string, data []byte) error {
	return WriteAtomically(path, data)
}
