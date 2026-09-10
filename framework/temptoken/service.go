package temptoken

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

// Errors returned by the service. Callers (notably the auth middleware) should
// treat these as opaque "not authorized" signals — they're typed so tests can
// assert on them without coupling to error message text.
var (
	// ErrTokenNotFound is returned when the presented token does not match any row.
	ErrTokenNotFound = errors.New("temptoken: token not found")
	// ErrTokenExpired is returned when the matched row's expires_at is in the past.
	ErrTokenExpired = errors.New("temptoken: token expired")
	// ErrScopeUnknown is returned when the row's scope column does not match any
	// registered scope. Indicates either a stale row (scope was deregistered) or
	// a corrupted row.
	ErrScopeUnknown = errors.New("temptoken: token scope is not registered")
	// ErrRouteNotAllowed is returned when the (method, path) of the request does
	// not satisfy any of the scope's AllowedRoutes (after resource_id substitution).
	ErrRouteNotAllowed = errors.New("temptoken: request method and path are not allowed by token scope")
	// ErrTTLExceedsMax is returned by Mint when the caller-requested TTL is
	// larger than the scope's MaxTTL.
	ErrTTLExceedsMax = errors.New("temptoken: requested TTL exceeds scope MaxTTL")
)

// ValidatedToken is the result of a successful Validate call. Callers attach
// these values to the request context so handlers can apply defense-in-depth
// checks.
type ValidatedToken struct {
	ID         string
	Scope      string
	ResourceID string
}

// Service mints and validates temp tokens. It composes the scope registry with
// the configstore-backed persistence layer; nothing else needs the row format
// directly.
type Service struct {
	registry      tenantstore.Resolver
	scopeRegistry *Registry
	now           func() time.Time // injectable for tests
}

// NewService constructs a Service backed by the tenant registry and scope registry.
func NewService(tenantRegistry tenantstore.Resolver, scopeReg *Registry) *Service {
	return &Service{registry: tenantRegistry, scopeRegistry: scopeReg, now: time.Now}
}

// NewServiceWithStore constructs a Service with a fixed store (for tests).
func NewServiceWithStore(store configstore.ConfigStore, scopeReg *Registry) *Service {
	return &Service{registry: tenantstore.NewStaticRegistry(store, ""), scopeRegistry: scopeReg, now: time.Now}
}

func (s *Service) store(ctx context.Context) configstore.ConfigStore {
	if s.registry == nil {
		return nil
	}
	return s.registry.GetStoreFromContext(ctx)
}

// Registry exposes the underlying scope registry so callers can register
// scopes without holding a separate reference. Useful for transports that
// receive only the Service from server startup.
func (s *Service) Registry() *Registry { return s.scopeRegistry }

// Mint creates a new temp token under the given scope, bound to resourceID,
// with the requested TTL. The TTL must be > 0 and <= the scope's MaxTTL. The
// returned plaintext is the value the caller embeds in URLs or hands back to
// the user; it is never persisted in plaintext when encryption is enabled.
func (s *Service) Mint(ctx context.Context, scopeName, resourceID string, ttl time.Duration) (string, error) {
	scope, ok := s.scopeRegistry.Lookup(scopeName)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrScopeUnknown, scopeName)
	}
	if ttl <= 0 || ttl > scope.MaxTTL {
		return "", fmt.Errorf("%w: requested=%s max=%s", ErrTTLExceedsMax, ttl, scope.MaxTTL)
	}
	plaintext, err := generatePlaintext()
	if err != nil {
		return "", fmt.Errorf("temptoken: failed to generate token: %w", err)
	}
	row := &tables.TempToken{
		ID:         uuid.New().String(),
		Token:      plaintext,
		Scope:      scope.Name,
		ResourceID: resourceID,
		ExpiresAt:  s.now().Add(ttl),
	}
	if store := s.store(ctx); store == nil {
		return "", fmt.Errorf("temptoken: tenant context required")
	} else if err := store.CreateTempToken(ctx, row); err != nil {
		return "", fmt.Errorf("temptoken: failed to persist token: %w", err)
	}
	return plaintext, nil
}

// Validate authenticates the presented plaintext for the given request
// (method, path).
func (s *Service) Validate(ctx context.Context, plaintext, method, path string) (*ValidatedToken, error) {
	if plaintext == "" {
		return nil, ErrTokenNotFound
	}
	hash := encrypt.HashSHA256(plaintext)
	store := s.store(ctx)
	if store == nil {
		return nil, fmt.Errorf("temptoken: tenant context required")
	}
	row, err := store.GetTempTokenByHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("temptoken: lookup failed: %w", err)
	}
	if row == nil {
		return nil, ErrTokenNotFound
	}
	if !row.ExpiresAt.After(s.now()) {
		return nil, ErrTokenExpired
	}
	scope, ok := s.scopeRegistry.Lookup(row.Scope)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrScopeUnknown, row.Scope)
	}
	if !scope.matchesRequest(method, path, row.ResourceID) {
		return nil, ErrRouteNotAllowed
	}
	return &ValidatedToken{
		ID:         row.ID,
		Scope:      row.Scope,
		ResourceID: row.ResourceID,
	}, nil
}

// DeleteExpired removes every token row whose expires_at is at or before
// `before`. The context must carry a tenant ID (set by TenantMiddleware on
// authenticated routes). Returns the number of rows removed.
func (s *Service) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	store := s.store(ctx)
	if store == nil {
		return 0, fmt.Errorf("temptoken: tenant context required")
	}
	n, err := store.DeleteExpiredTempTokens(ctx, before)
	if err != nil {
		return 0, fmt.Errorf("temptoken: delete expired failed: %w", err)
	}
	return n, nil
}

// DeleteExpiredAll iterates every registered tenant and reaps expired rows.
// Called by SweepWorker which runs under a background context with no tenant ID.
func (s *Service) DeleteExpiredAll(ctx context.Context, before time.Time) (int64, error) {
	if s.registry == nil {
		return 0, nil
	}
	var total int64
	for _, tenantID := range s.registry.ListTenantIDs(ctx) {
		store := s.registry.PeekStoreForTenant(tenantID)
		if store == nil {
			continue
		}
		tenantCtx := context.WithValue(ctx, schemas.BifrostContextKeyTenantID, tenantID)
		n, err := store.DeleteExpiredTempTokens(tenantCtx, before)
		if err != nil {
			return total, fmt.Errorf("temptoken: delete expired (tenant %s): %w", tenantID, err)
		}
		total += n
	}
	return total, nil
}

// DeleteByResourceID removes every token row matching (scope, resourceID).
// Lifecycle owners call this when the underlying resource the token authorized
// is finished — e.g. the OAuth provider after a per-user flow terminates
// (success or failure) so the link stops working immediately instead of
// waiting for TTL. Returns the number of rows removed; both 0 and N are
// considered successful outcomes — callers should not treat 0 as an error.
func (s *Service) DeleteByResourceID(ctx context.Context, scope, resourceID string) (int64, error) {
	if scope == "" || resourceID == "" {
		return 0, nil
	}
	store := s.store(ctx)
	if store == nil {
		return 0, fmt.Errorf("temptoken: tenant context required")
	}
	n, err := store.DeleteTempTokensByResourceID(ctx, scope, resourceID)
	if err != nil {
		return 0, fmt.Errorf("temptoken: delete by resource_id failed: %w", err)
	}
	return n, nil
}

// generatePlaintext returns a cryptographically random URL-safe string. 32
// bytes of entropy yields ~43 base64url characters — plenty against any
// realistic brute-force budget given the 15-minute TTL ceiling.
func generatePlaintext() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
