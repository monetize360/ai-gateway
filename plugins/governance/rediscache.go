package governance

import (
	"container/list"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
	load(ctx context.Context, tenantID string) (*redisGovernanceSnapshot, error)
	start(parent context.Context, onChanged func(string, int64))
	stop()
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
			subscription := c.client.Subscribe(ctx, c.channel)
			channel := subscription.Channel()
			for {
				select {
				case <-ctx.Done():
					_ = subscription.Close()
					return
				case message, ok := <-channel:
					if !ok {
						_ = subscription.Close()
						time.Sleep(time.Second)
						goto reconnect
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
		reconnect:
		}
	}()
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

func (c *redisGovernanceCache) load(ctx context.Context, tenantID string) (*redisGovernanceSnapshot, error) {
	if cached, ok := c.local.get(tenantID); ok {
		version, err := c.version(ctx, tenantID)
		if err == nil && version == cached.Version {
			return cached, nil
		}
		c.local.remove(tenantID)
	}
	version, err := c.version(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		return nil, nil
	}
	snapshot := &redisGovernanceSnapshot{Version: version}
	if err := c.readEntities(ctx, tenantID, "organizations", &snapshot.Organizations); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "virtual_keys", &snapshot.VirtualKeys); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "budgets", &snapshot.Budgets); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "budget_usages", &snapshot.BudgetUsages); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "accounts", &snapshot.Accounts); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "wallets", &snapshot.Wallets); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "org_units", &snapshot.OrgUnits); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "rate_limits", &snapshot.RateLimits); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "providers", &snapshot.Providers); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "models", &snapshot.ConfigModels); err != nil {
		return nil, err
	}
	if err := c.readEntities(ctx, tenantID, "routing_rules", &snapshot.RoutingRules); err != nil {
		return nil, err
	}
	for _, model := range snapshot.ConfigModels {
		snapshot.ModelConfigs = append(snapshot.ModelConfigs, *configstoreTables.ModelConfigFromTableModel(&model, nil))
	}
	c.local.put(tenantID, snapshot)
	return snapshot, nil
}

func (c *redisGovernanceCache) version(ctx context.Context, tenantID string) (int64, error) {
	return c.client.Get(ctx, c.prefix+":tenant:"+tenantID+":version").Int64()
}

func (c *redisGovernanceCache) readEntities(ctx context.Context, tenantID, kind string, target any) error {
	values, err := c.client.HVals(ctx, c.prefix+":tenant:"+tenantID+":entities:"+kind).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}
	records := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(value), &envelope); err != nil || len(envelope.Data) == 0 {
			continue
		}
		records = append(records, envelope.Data)
	}
	body, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
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
	snapshot *redisGovernanceSnapshot
	expires  time.Time
}

func newTenantSnapshotLRU(capacity int, ttl time.Duration) *tenantSnapshotLRU {
	return &tenantSnapshotLRU{capacity: capacity, ttl: ttl, items: make(map[string]*list.Element), order: list.New()}
}

func (l *tenantSnapshotLRU) get(tenantID string) (*redisGovernanceSnapshot, bool) {
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

func (l *tenantSnapshotLRU) put(tenantID string, snapshot *redisGovernanceSnapshot) {
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
