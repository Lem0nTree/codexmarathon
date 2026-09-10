package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountsImportUsesExistingAuthWithoutRuntime(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "opaque-stock-token"
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"account_id":"stock-account","access_token":"`+secret+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"accounts", "import",
		"--state-dir", filepath.Join(root, "state"),
		"--auth", authPath,
		"--name", "personal",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("accounts import exit code = %d, stderr = %s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "imported and activated account: stock-account") {
		t.Fatalf("accounts import output = %q", got)
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatalf("accounts import exposed auth material: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "state", "credentials", "stock-account.json")); err != nil {
		t.Fatalf("imported vault snapshot missing: %v", err)
	}
}
