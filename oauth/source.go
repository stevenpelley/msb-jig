package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Store keeps the refresh token (the Keychain, on the Mac).
type Store interface {
	Load() (string, error)
	Save(refresh string) error
}

// Source is the broker's access token for one server: kept in memory
// until a minute before it expires, then refreshed with the stored
// refresh token. A rotated refresh token is saved before the new access
// token is used. Lock serializes refreshes across processes (each
// sandbox's broker is its own): the refresh token is read inside it, so
// one process never spends a token another already rotated.
type Source struct {
	TokenEndpoint, ClientID, Resource string
	Store                             Store
	Lock                              func() (unlock func(), err error)
	Client                            *http.Client
	// Login is the command that signs in again, for messages.
	Login string

	mu      sync.Mutex
	access  string
	expires time.Time
}

// Token returns a current access token (a broker.Token).
func (s *Source) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.access != "" && time.Until(s.expires) > time.Minute {
		return s.access, nil
	}
	unlock, err := s.Lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	refresh, err := s.Store.Load()
	if err != nil || refresh == "" {
		return "", fmt.Errorf("no refresh token: run %s", s.Login)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := Refresh(ctx, s.Client, s.TokenEndpoint, s.ClientID, s.Resource, refresh)
	if errors.Is(err, ErrInvalidGrant) {
		return "", fmt.Errorf("the sign-in has expired or was revoked: run %s", s.Login)
	}
	if err != nil {
		return "", err
	}
	if t.Refresh != "" && t.Refresh != refresh {
		if err := s.Store.Save(t.Refresh); err != nil {
			return "", fmt.Errorf("saving the rotated refresh token: %w (run %s)", err, s.Login)
		}
	}
	s.access, s.expires = t.Access, t.Expires
	return s.access, nil
}
