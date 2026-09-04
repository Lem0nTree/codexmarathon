package quota

import (
	"context"
	"encoding/json"
	"errors"

	"codexmarathon/controller/internal/credentials"
)

// VaultCredentialSource adapts CodexMarathon's opaque vault to the quota
// client's bearer-token seam. Parsing is deliberately limited to the fields
// required for the donor-compatible calibration request; all unknown auth
// fields remain owned by the embedded Codex runtime.
type VaultCredentialSource struct {
	Vault credentials.SnapshotReader
}

func (s VaultCredentialSource) Tokens(ctx context.Context, accountID string) (Tokens, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Tokens{}, err
	}
	if s.Vault == nil {
		return Tokens{}, errors.New("credential vault is unavailable")
	}
	raw, err := s.Vault.Load(accountID)
	if err != nil {
		return Tokens{}, errors.New("credential snapshot is unavailable")
	}
	tokens, err := extractTokens(raw)
	if err != nil {
		return Tokens{}, err
	}
	if tokens.AccessToken == "" {
		return Tokens{}, ErrMissingToken
	}
	return tokens, nil
}

func extractTokens(raw []byte) (Tokens, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return Tokens{}, errors.New("credential snapshot is not a JSON object")
	}
	var tokensObject map[string]json.RawMessage
	if nested, ok := object["tokens"]; ok && string(nested) != "null" {
		if err := json.Unmarshal(nested, &tokensObject); err != nil || tokensObject == nil {
			return Tokens{}, errors.New("credential snapshot tokens are invalid")
		}
	} else {
		tokensObject = object
	}
	var tokens Tokens
	if value, ok := tokensObject["access_token"]; ok {
		_ = json.Unmarshal(value, &tokens.AccessToken)
	}
	if value, ok := tokensObject["account_id"]; ok {
		_ = json.Unmarshal(value, &tokens.AccountID)
	}
	if tokens.AccountID == "" {
		if value, ok := object["account_id"]; ok {
			_ = json.Unmarshal(value, &tokens.AccountID)
		}
	}
	return tokens, nil
}
