package governance

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/redis/go-redis/v9"
)

const (
	defaultGovernanceCachePrefix  = "mpilot:ai-gateway:governance"
	defaultGovernanceCacheChannel = "mpilot:ai-gateway:governance:changed"
)

// RedisCacheConfig is deliberately transport-agnostic so the governance plugin
// can be used outside the HTTP server too.
type RedisCacheConfig struct {
	Address        string
	Username       string
	Password       string
	Database       int
	TLS            bool
	KeyPrefix      string
	Channel        string
	LRUCapacity    int
	LRUTTL         time.Duration
	ConnectTimeout time.Duration
}

type governanceCacheEvent struct {
	TenantID string `json:"tenant_id"`
	Version  int64  `json:"version"`
}

// governanceSnapshotCache is the configuration cache consulted before Postgres.
// *redisGovernanceCache is the production implementation.
type governanceSnapshotCache interface {
	// load returns a freshly decoded snapshot (callers may mutate it), or nil
	// when the tenant has no Redis namespace yet.
	load(ctx context.Context, tenantID string) (*redisGovernanceSnapshot, error)
	// versions returns the published version per tenant; 0 means not seeded.
	versions(ctx context.Context, tenantIDs []string) (map[string]int64, error)
	start(parent context.Context, onChanged func(string, int64))
	stop()
}

// redisGovernanceEntityKinds is the fixed set of entity hashes MPilot writes per tenant.
var redisGovernanceEntityKinds = []string{
	"organizations", "virtual_keys", "budgets", "budget_usages", "accounts", "wallets",
	"org_units", "rate_limits", "providers", "models", "routing_rules",
}

// redisGovernanceCache reads the entity hashes written by MPilot. Pub/sub is an
// acceleration path; the monotonic version key makes a missed notification safe.
type redisGovernanceCache struct {
	client  *redis.Client
	prefix  string
	channel string
	logger  interface {
		Debug(string, ...any)
		Warn(string, ...any)
	}
	local     *tenantSnapshotLRU
	cancel    context.CancelFunc
	onChanged func(string, int64)
}

type redisGovernanceSnapshot struct {
	Version       int64
	Organizations []configstoreTables.TableOrganization
	VirtualKeys   []configstoreTables.TableVirtualKey
	Budgets       []configstoreTables.TableBudget
	BudgetUsages  []configstoreTables.TableBudgetUsage
	Accounts      []configstoreTables.TableAccount
	Wallets       []configstoreTables.TableWallet
	OrgUnits      []configstoreTables.TableOrgUnit
	RateLimits    []configstoreTables.TableRateLimit
	ModelConfigs  []configstoreTables.TableModelConfig
	Providers     []configstoreTables.TableProvider
	ConfigModels  []configstoreTables.TableModel
	RoutingRules  []configstoreTables.TableRoutingRule
}

// redisGovernanceRaw is what the LRU keeps: the JSON array per entity kind.
// The live store keeps pointers into the slices it is given and mutates them
// (usage counters, reverse-FK attachments), so every load decodes a fresh copy
// instead of sharing one decoded snapshot.
type redisGovernanceRaw struct {
	Version  int64
	Entities map[string][]byte
}

func newRedisGovernanceCache(cfg RedisCacheConfig, logger interface {
	Debug(string, ...any)
	Warn(string, ...any)
}) (*redisGovernanceCache, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("governance Redis cache address is required")
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = defaultGovernanceCachePrefix
	}
	if cfg.Channel == "" {
		cfg.Channel = defaultGovernanceCacheChannel
	}
	if cfg.LRUCapacity <= 0 {
		cfg.LRUCapacity = 2000
	}
	if cfg.LRUTTL <= 0 {
		cfg.LRUTTL = 5 * time.Minute
	}
	options := &redis.Options{
		Addr:        cfg.Address,
		Username:    cfg.Username,
		Password:    cfg.Password,
		DB:          cfg.Database,
		DialTimeout: cfg.ConnectTimeout,
	}
	if cfg.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &redisGovernanceCache{
		client:  redis.NewClient(options),
		prefix:  cfg.KeyPrefix,
		channel: cfg.Channel,
		logger:  logger,
		local:   newTenantSnapshotLRU(cfg.LRUCapacity, cfg.LRUTTL),
	}, nil
}

func (c *redisGovernanceCache) start(parent context.Context, onChanged func(string, int64)) {
	if c == nil || c.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.onChanged = onChanged
	go func() {
		for ctx.Err() == nil {
			c.consume(ctx)
			// The subscription closed. Back off before resubscribing; the
			// reconcile loop recovers anything published in the gap.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

func (c *redisGovernanceCache) consume(ctx context.Context) {
	subscription := c.client.Subscribe(ctx, c.channel)
	defer subscription.Close()
	channel := subscription.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-channel:
			if !ok {
				return
			}
			var event governanceCacheEvent
			if err := json.Unmarshal([]byte(message.Payload), &event); err != nil || event.TenantID == "" {
				continue
			}
			c.local.remove(event.TenantID)
			if c.onChanged != nil {
				c.onChanged(event.TenantID, event.Version)
			}
		}
	}
}

func (c *redisGovernanceCache) stop() {
	if c == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	_ = c.client.Close()
}

func (c *redisGovernanceCache) versionKey(tenantID string) string {
	return c.prefix + ":tenant:" + tenantID + ":version"
}

func (c *redisGovernanceCache) load(ctx context.Context, tenantID string) (*redisGovernanceSnapshot, error) {
	version, err := c.version(ctx, tenantID)
	if err != nil {
		// Keep the cached copy: Redis being unreachable is not a reason to drop it.
		return nil, err
	}
	if version == 0 {
		c.local.remove(tenantID)
		return nil, nil
	}
	if cached, ok := c.local.get(tenantID); ok && cached.Version == version {
		return decodeGovernanceSnapshot(cached)
	}
	raw, err := c.readEntities(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	raw.Version = version
	c.local.put(tenantID, raw)
	return decodeGovernanceSnapshot(raw)
}

// version returns 0 when the tenant has not been seeded (redis.Nil).
func (c *redisGovernanceCache) version(ctx context.Context, tenantID string) (int64, error) {
	version, err := c.client.Get(ctx, c.versionKey(tenantID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return version, err
}

func (c *redisGovernanceCache) versions(ctx context.Context, tenantIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(tenantIDs))
	if len(tenantIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(tenantIDs))
	for i, tenantID := range tenantIDs {
		keys[i] = c.versionKey(tenantID)
	}
	values, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		var version int64
		switch v := value.(type) {
		case string:
			version, _ = strconv.ParseInt(v, 10, 64)
		case int64:
			version = v
		}
		out[tenantIDs[i]] = version
	}
	return out, nil
}

// readEntities fetches every entity hash in one pipelined round trip and turns
// each into a JSON array of the envelope "data" payloads.
func (c *redisGovernanceCache) readEntities(ctx context.Context, tenantID string) (*redisGovernanceRaw, error) {
	pipe := c.client.Pipeline()
	commands := make([]*redis.StringSliceCmd, len(redisGovernanceEntityKinds))
	for i, kind := range redisGovernanceEntityKinds {
		commands[i] = pipe.HVals(ctx, c.prefix+":tenant:"+tenantID+":entities:"+kind)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	raw := &redisGovernanceRaw{Entities: make(map[string][]byte, len(commands))}
	for i, kind := range redisGovernanceEntityKinds {
		values, err := commands[i].Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		count := 0
		for _, value := range values {
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(value), &envelope); err != nil || len(envelope.Data) == 0 {
				continue
			}
			if count > 0 {
				buf.WriteByte(',')
			}
			buf.Write(envelope.Data)
			count++
		}
		buf.WriteByte(']')
		raw.Entities[kind] = buf.Bytes()
	}
	return raw, nil
}

func decodeGovernanceSnapshot(raw *redisGovernanceRaw) (*redisGovernanceSnapshot, error) {
	snapshot := &redisGovernanceSnapshot{Version: raw.Version}
	targets := map[string]any{
		"organizations": &snapshot.Organizations,
		"virtual_keys":  &snapshot.VirtualKeys,
		"budgets":       &snapshot.Budgets,
		"budget_usages": &snapshot.BudgetUsages,
		"accounts":      &snapshot.Accounts,
		"wallets":       &snapshot.Wallets,
		"org_units":     &snapshot.OrgUnits,
		"rate_limits":   &snapshot.RateLimits,
		"providers":     &snapshot.Providers,
		"models":        &snapshot.ConfigModels,
		"routing_rules": &snapshot.RoutingRules,
	}
	for kind, target := range targets {
		body := raw.Entities[kind]
		if len(body) == 0 {
			continue
		}
		if err := json.Unmarshal(body, target); err != nil {
			return nil, fmt.Errorf("decode governance %s: %w", kind, err)
		}
	}
	for i := range snapshot.ConfigModels {
		snapshot.ModelConfigs = append(snapshot.ModelConfigs, *configstoreTables.ModelConfigFromTableModel(&snapshot.ConfigModels[i], nil))
	}
	return snapshot, nil
}

type tenantSnapshotLRU struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*list.Element
	order    *list.List
}

type tenantSnapshotEntry struct {
	tenantID string
	snapshot *redisGovernanceRaw
	expires  time.Time
}

func newTenantSnapshotLRU(capacity int, ttl time.Duration) *tenantSnapshotLRU {
	return &tenantSnapshotLRU{capacity: capacity, ttl: ttl, items: make(map[string]*list.Element), order: list.New()}
}

func (l *tenantSnapshotLRU) get(tenantID string) (*redisGovernanceRaw, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	element, ok := l.items[tenantID]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*tenantSnapshotEntry)
	if time.Now().After(entry.expires) {
		l.order.Remove(element)
		delete(l.items, tenantID)
		return nil, false
	}
	l.order.MoveToFront(element)
	return entry.snapshot, true
}

func (l *tenantSnapshotLRU) put(tenantID string, snapshot *redisGovernanceRaw) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.items[tenantID]; ok {
		entry := existing.Value.(*tenantSnapshotEntry)
		entry.snapshot = snapshot
		entry.expires = time.Now().Add(l.ttl)
		l.order.MoveToFront(existing)
		return
	}
	element := l.order.PushFront(&tenantSnapshotEntry{tenantID: tenantID, snapshot: snapshot, expires: time.Now().Add(l.ttl)})
	l.items[tenantID] = element
	if l.order.Len() > l.capacity {
		tail := l.order.Back()
		delete(l.items, tail.Value.(*tenantSnapshotEntry).tenantID)
		l.order.Remove(tail)
	}
}

func (l *tenantSnapshotLRU) remove(tenantID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.items[tenantID]; ok {
		l.order.Remove(element)
		delete(l.items, tenantID)
	}
}
