package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/tenantstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib/kafkainject"
	"github.com/valyala/fasthttp"
)

type jwtCacheEntry struct {
	tenantID  string
	expiresAt time.Time
}

// IngestAuthMiddleware verifies tenant JWTs (tenantId only, no virtualKey) with a short TTL cache.
type IngestAuthMiddleware struct {
	adminJWTKey []byte
	mu          sync.RWMutex
	cache       map[string]jwtCacheEntry
}

// NewIngestAuthMiddleware creates middleware that accepts tenant admin JWTs without virtualKey.
func NewIngestAuthMiddleware(adminJWTKey []byte) *IngestAuthMiddleware {
	return &IngestAuthMiddleware{
		adminJWTKey: adminJWTKey,
		cache:       make(map[string]jwtCacheEntry),
	}
}

// Middleware returns a fasthttp middleware that sets BifrostContextKeyTenantID.
func (m *IngestAuthMiddleware) Middleware() schemas.BifrostHTTPMiddleware {
	return func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) {
			if err := m.authenticate(ctx); err != nil {
				SendError(ctx, fasthttp.StatusUnauthorized, err.Error())
				return
			}
			next(ctx)
		}
	}
}

func (m *IngestAuthMiddleware) authenticate(ctx *fasthttp.RequestCtx) error {
	authHeader := string(ctx.Request.Header.Peek("Authorization"))
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return fmt.Errorf("missing or invalid Authorization header")
	}
	rawToken := strings.TrimPrefix(authHeader, "Bearer ")
	if rawToken == "" {
		return fmt.Errorf("missing or invalid Authorization header")
	}

	tokenHash := hashToken(rawToken)
	now := time.Now()

	m.mu.RLock()
	if entry, ok := m.cache[tokenHash]; ok && now.Before(entry.expiresAt) {
		tenantID := entry.tenantID
		m.mu.RUnlock()
		ctx.SetUserValue(schemas.BifrostContextKeyTenantID, tenantID)
		return nil
	}
	m.mu.RUnlock()

	if len(m.adminJWTKey) == 0 {
		return fmt.Errorf("tenant admin JWT key is not configured")
	}
	claims, err := tenantstore.ExtractTenantAuthClaimsFromJWT(rawToken, m.adminJWTKey)
	if err != nil {
		return fmt.Errorf("invalid tenant token: %w", err)
	}
	if claims.TenantID == "" {
		return fmt.Errorf("token missing tenantId claim")
	}

	expiresAt := now.Add(kafkainject.JWTCacheTTL)
	if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(expiresAt) {
		expiresAt = claims.ExpiresAt.Time
	}

	m.mu.Lock()
	m.cache[tokenHash] = jwtCacheEntry{tenantID: claims.TenantID, expiresAt: expiresAt}
	// Opportunistic cleanup of a few expired entries to bound map growth.
	if len(m.cache) > 10_000 {
		for k, v := range m.cache {
			if now.After(v.expiresAt) {
				delete(m.cache, k)
			}
		}
	}
	m.mu.Unlock()

	ctx.SetUserValue(schemas.BifrostContextKeyTenantID, claims.TenantID)
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
