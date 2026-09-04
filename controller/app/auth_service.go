package app

import (
	"context"
	"errors"

	"codexmarathon/controller/internal/accounts"
	"codexmarathon/controller/internal/credentials"
	"codexmarathon/controller/internal/runtime"
)

// RuntimeAuthService adapts the embedded Codex runtime's native login and
// AuthManager refresh calls to the controller account manager. The client
// keeps auth material as an opaque in-memory JSON-RPC value; this adapter never
// logs or exposes it and the manager persists it only in its protected vault.
type RuntimeAuthService struct {
	Client *runtime.Client
}

// NewRuntimeAuthService constructs the direct controller/runtime auth seam.
// The runtime client must already have completed protocol negotiation.
func NewRuntimeAuthService(client *runtime.Client) RuntimeAuthService {
	return RuntimeAuthService{Client: client}
}

func (s RuntimeAuthService) Login(ctx context.Context, request accounts.LoginRequest) (accounts.LoginResult, error) {
	if s.Client == nil {
		return accounts.LoginResult{}, accounts.ErrAuthServiceUnavailable
	}
	result, err := s.Client.LoginAccount(ctx, runtime.NativeLoginParams{
		AccountID: request.AccountID,
		Alias:     request.Alias,
		Overwrite: request.Overwrite,
	})
	if err != nil {
		return accounts.LoginResult{}, err
	}
	return accounts.LoginResult{
		AccountID: result.AccountID,
		Alias:     result.Alias,
		AuthJSON:  result.AuthJSON,
		Metadata:  result.Metadata,
	}, nil
}

func (s RuntimeAuthService) Refresh(ctx context.Context, request accounts.RefreshRequest) (credentials.TokenSet, error) {
	if s.Client == nil {
		return credentials.TokenSet{}, accounts.ErrAuthServiceUnavailable
	}
	result, err := s.Client.RefreshAccount(ctx, runtime.NativeRefreshParams{
		AccountID: request.AccountID,
		AuthJSON:  request.AuthJSON,
	})
	if err != nil {
		return credentials.TokenSet{}, err
	}
	if result.AccessToken == "" && result.IDToken == "" && result.RefreshToken == "" {
		return credentials.TokenSet{}, errors.New("native refresh returned no updated token fields")
	}
	return credentials.TokenSet{
		AccessToken:  result.AccessToken,
		IDToken:      result.IDToken,
		RefreshToken: result.RefreshToken,
		AccountID:    result.AccountID,
	}, nil
}

var _ accounts.AuthService = RuntimeAuthService{}
