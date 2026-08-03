package kafkainject

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/tenantstore"
)

// FieldKind is a simplified JSON type used for lightweight validation.
type FieldKind string

const (
	KindString  FieldKind = "string"
	KindInteger FieldKind = "integer"
	KindNumber  FieldKind = "number"
	KindBoolean FieldKind = "boolean"
	KindObject  FieldKind = "object"
	KindArray   FieldKind = "array"
)

// SchemaCache caches compiled mObject field maps.
type SchemaCache struct {
	registry tenantstore.Resolver
	mu       sync.RWMutex
	entries  map[string]*schemaEntry
}

type schemaEntry struct {
	fields   map[string]FieldKind
	loadedAt time.Time
}

// NewSchemaCache creates an empty schema cache.
func NewSchemaCache(registry tenantstore.Resolver) *SchemaCache {
	return &SchemaCache{
		registry: registry,
		entries:  make(map[string]*schemaEntry),
	}
}

func schemaKey(tenantID, mObjectID string) string {
	return tenantID + ":" + mObjectID
}

// GetOrLoad returns the field map for an mObject, loading from tenant DB on miss/expiry.
func (c *SchemaCache) GetOrLoad(ctx context.Context, tenantID, mObjectID string) (map[string]FieldKind, error) {
	key := schemaKey(tenantID, mObjectID)

	c.mu.RLock()
	if e, ok := c.entries[key]; ok && time.Since(e.loadedAt) < SchemaCacheTTL {
		fields := e.fields
		c.mu.RUnlock()
		return fields, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && time.Since(e.loadedAt) < SchemaCacheTTL {
		return e.fields, nil
	}

	fields, err := c.load(ctx, tenantID, mObjectID)
	if err != nil {
		return nil, err
	}
	c.entries[key] = &schemaEntry{fields: fields, loadedAt: time.Now()}
	return fields, nil
}

func (c *SchemaCache) load(ctx context.Context, tenantID, mObjectID string) (map[string]FieldKind, error) {
	if c.registry == nil {
		return nil, fmt.Errorf("tenant registry not configured")
	}
	store := c.registry.GetStoreForTenant(ctx, tenantID)
	if store == nil {
		return nil, fmt.Errorf("unknown tenant")
	}
	return loadMObjectFields(ctx, store, mObjectID)
}

type mobjectSchemaRow struct {
	Schema json.RawMessage `gorm:"column:schema"`
}

type jsonSchemaDoc struct {
	Properties map[string]jsonSchemaProp `json:"properties"`
}

type jsonSchemaProp struct {
	Type   any    `json:"type"`
	Format string `json:"format"`
}

func loadMObjectFields(ctx context.Context, store configstore.ConfigStore, mObjectID string) (map[string]FieldKind, error) {
	var row mobjectSchemaRow
	err := store.DB().WithContext(ctx).Raw(`
		SELECT schema
		FROM mobject
		WHERE id = ?::uuid
		  AND COALESCE(deleted, false) = false
		LIMIT 1
	`, mObjectID).Scan(&row).Error
	if err != nil {
		return nil, fmt.Errorf("failed to load mobject schema: %w", err)
	}
	if len(row.Schema) == 0 {
		return nil, fmt.Errorf("mobject not found or schema empty: %s", mObjectID)
	}

	var doc jsonSchemaDoc
	if err := json.Unmarshal(row.Schema, &doc); err != nil {
		return nil, fmt.Errorf("invalid mobject schema json: %w", err)
	}
	fields := make(map[string]FieldKind, len(doc.Properties))
	for name, prop := range doc.Properties {
		fields[name] = mapPropKind(prop)
	}
	return fields, nil
}

func mapPropKind(prop jsonSchemaProp) FieldKind {
	typeStr := strings.ToLower(firstType(prop.Type))
	switch typeStr {
	case "integer":
		return KindInteger
	case "number":
		return KindNumber
	case "boolean":
		return KindBoolean
	case "object":
		return KindObject
	case "array":
		return KindArray
	case "string", "reference", "picklist", "currency", "date", "datetime", "date-time":
		return KindString
	default:
		return KindString
	}
}

func firstType(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s != "null" {
				return s
			}
		}
	}
	return "string"
}

// ValidateMessage checks message keys against the schema field map (names + datatypes only).
func ValidateMessage(message map[string]any, fields map[string]FieldKind) error {
	if message == nil {
		return fmt.Errorf("message is required")
	}
	for key, value := range message {
		kind, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown field %s", key)
		}
		if value == nil {
			continue
		}
		if err := checkType(key, value, kind); err != nil {
			return err
		}
	}
	return nil
}

func checkType(field string, value any, kind FieldKind) error {
	switch kind {
	case KindString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("field %s expected string", field)
		}
	case KindBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("field %s expected boolean", field)
		}
	case KindInteger:
		switch n := value.(type) {
		case float64:
			if n != float64(int64(n)) {
				return fmt.Errorf("field %s expected integer", field)
			}
		case json.Number:
			if _, err := n.Int64(); err != nil {
				return fmt.Errorf("field %s expected integer", field)
			}
		default:
			return fmt.Errorf("field %s expected integer", field)
		}
	case KindNumber:
		switch value.(type) {
		case float64, json.Number:
			return nil
		default:
			return fmt.Errorf("field %s expected number", field)
		}
	case KindObject:
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("field %s expected object", field)
		}
	case KindArray:
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("field %s expected array", field)
		}
	}
	return nil
}
