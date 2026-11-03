package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/tenantstore"
	"github.com/maximhq/bifrost/plugins/governance"
	"github.com/maximhq/bifrost/transports/bifrost-http/handlers"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// governanceTestTenantID is the tenant InitFromStore installs in memory.
const governanceTestTenantID = "__bifrost_test_tenant__"

var inferenceTestSecret = []byte("inference-auth-cache-test-secret")

// inferenceVirtualKeyCounter counts configstore.GetVirtualKey. That call is the
// Postgres preload that ran on every inference request before the resolver.
type inferenceVirtualKeyCounter struct {
	configstore.ConfigStore
	mu   sync.Mutex
	gets int
	keys map[string]*configstoreTables.TableVirtualKey
}

func (s *inferenceVirtualKeyCounter) GetVirtualKey(_ context.Context, id string) (*configstoreTables.TableVirtualKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	vk, ok := s.keys[id]
	if !ok || vk == nil {
		return nil, configstore.ErrNotFound
	}
	clone := *vk
	return &clone, nil
}

func (s *inferenceVirtualKeyCounter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

type inferenceTenantRegistry struct {
	store configstore.ConfigStore
}

func (r *inferenceTenantRegistry) GetStoreFromContext(context.Context) configstore.ConfigStore {
	return r.store
}
func (r *inferenceTenantRegistry) GetStoreFromRequestCtx(*fasthttp.RequestCtx) configstore.ConfigStore {
	return r.store
}
func (r *inferenceTenantRegistry) RequireStoreFromRequestCtx(*fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	return r.store, nil
}
func (r *inferenceTenantRegistry) ListTenantIDs(context.Context) []string { return nil }
func (r *inferenceTenantRegistry) SyncTenants(context.Context) error      { return nil }
func (r *inferenceTenantRegistry) GetStoreForTenant(context.Context, string) configstore.ConfigStore {
	return r.store
}
func (r *inferenceTenantRegistry) PeekStoreForTenant(string) configstore.ConfigStore {
	return r.store
}

func inferenceVirtualKey(id, name string, active bool) *configstoreTables.TableVirtualKey {
	return &configstoreTables.TableVirtualKey{
		ID:       id,
		Name:     name,
		Value:    id,
		IsActive: &active,
	}
}

func signInferenceToken(t *testing.T, tenantID, virtualKeyID string) string {
	t.Helper()
	claims := tenantstore.TenantClaims{
		TenantID:   tenantID,
		VirtualKey: virtualKeyID,
		MorgID:     "org-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(inferenceTestSecret)
	require.NoError(t, err)
	return signed
}

func newInferencePlugin(t *testing.T, keys ...*configstoreTables.TableVirtualKey) *governance.GovernancePlugin {
	t.Helper()
	rows := make([]configstoreTables.TableVirtualKey, 0, len(keys))
	for _, key := range keys {
		if key != nil {
			rows = append(rows, *key)
		}
	}
	store, err := governance.NewLocalGovernanceStore(context.Background(), noopTestLogger{}, nil, &configstore.GovernanceConfig{
		VirtualKeys: rows,
	}, nil)
	require.NoError(t, err)
	plugin, err := governance.InitFromStore(context.Background(), &governance.Config{}, noopTestLogger{}, store, nil, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = plugin.Cleanup()
	})
	return plugin
}

func serveInference(token string, middleware *handlers.TenantMiddleware) *fasthttp.RequestCtx {
	var ctx fasthttp.RequestCtx
	if token != "" {
		ctx.Request.Header.Set("Authorization", "Bearer "+token)
	}
	handler := middleware.Middleware()(func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusNoContent)
	})
	handler(&ctx)
	return &ctx
}

func TestInferenceAuth_WarmVirtualKeyDoesNotQueryPostgres(t *testing.T) {
	vk := inferenceVirtualKey("vk-warm", "warm", true)
	db := &inferenceVirtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	plugin := newInferencePlugin(t, vk)
	middleware := handlers.NewTenantMiddleware(inferenceTestSecret, &inferenceTenantRegistry{store: db})
	middleware.SetVirtualKeyResolver(plugin)
	token := signInferenceToken(t, governanceTestTenantID, vk.ID)

	first := serveInference(token, middleware)
	second := serveInference(token, middleware)

	require.Equal(t, fasthttp.StatusNoContent, first.Response.StatusCode())
	require.Equal(t, fasthttp.StatusNoContent, second.Response.StatusCode())
	require.Equal(t, governanceTestTenantID, first.UserValue(schemas.BifrostContextKeyTenantID))
	require.Equal(t, vk.ID, first.UserValue(schemas.BifrostContextKeyGovernanceVirtualKeyID))
	require.Equal(t, "org-1", first.UserValue(schemas.BifrostContextKeyGovernanceOrgID))
	require.Equal(t, 0, db.count(), "inference auth must resolve a warm virtual key without GetVirtualKey")
}

func TestInferenceAuth_WithoutResolverQueriesPostgresEveryRequest(t *testing.T) {
	vk := inferenceVirtualKey("vk-db", "from-postgres", true)
	db := &inferenceVirtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	middleware := handlers.NewTenantMiddleware(inferenceTestSecret, &inferenceTenantRegistry{store: db})
	token := signInferenceToken(t, governanceTestTenantID, vk.ID)

	first := serveInference(token, middleware)
	second := serveInference(token, middleware)

	require.Equal(t, fasthttp.StatusNoContent, first.Response.StatusCode())
	require.Equal(t, fasthttp.StatusNoContent, second.Response.StatusCode())
	require.Equal(t, 2, db.count(), "without the governance resolver every inference request calls GetVirtualKey")
}

func TestInferenceAuth_InactiveVirtualKeyRejectsWithoutPostgres(t *testing.T) {
	vk := inferenceVirtualKey("vk-off", "inactive", false)
	db := &inferenceVirtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	plugin := newInferencePlugin(t, vk)
	middleware := handlers.NewTenantMiddleware(inferenceTestSecret, &inferenceTenantRegistry{store: db})
	middleware.SetVirtualKeyResolver(plugin)

	ctx := serveInference(signInferenceToken(t, governanceTestTenantID, vk.ID), middleware)

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Contains(t, string(ctx.Response.Body()), "virtual key is inactive")
	require.Equal(t, 0, db.count())
}

func TestInferenceAuth_ResolverMissDoesNotUseMiddlewareStore(t *testing.T) {
	vk := inferenceVirtualKey("vk-only-db", "from-postgres", true)
	db := &inferenceVirtualKeyCounter{keys: map[string]*configstoreTables.TableVirtualKey{vk.ID: vk}}
	plugin := newInferencePlugin(t)
	middleware := handlers.NewTenantMiddleware(inferenceTestSecret, &inferenceTenantRegistry{store: db})
	middleware.SetVirtualKeyResolver(plugin)

	ctx := serveInference(signInferenceToken(t, governanceTestTenantID, vk.ID), middleware)

	require.Equal(t, fasthttp.StatusUnauthorized, ctx.Response.StatusCode())
	require.Contains(t, string(ctx.Response.Body()), "virtual key not found")
	require.Equal(t, 0, db.count(), "a resolver miss must not fall through to the middleware config store")
}

var _ tenantstore.Resolver = (*inferenceTenantRegistry)(nil)
