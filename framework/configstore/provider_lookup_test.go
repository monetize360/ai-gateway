package configstore

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
)

func TestScopeProviderByRuntimeKey_StandardProviderUsesProviderType(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	openaiType := "d2803e2c-5ae8-4496-a264-c979c5be5d30"
	provider := tables.TableProvider{
		Name:         "stale-name",
		ProviderType: &openaiType,
	}
	require.NoError(t, store.DB().Create(&provider).Error)

	cfg, err := store.GetProviderConfig(ctx, schemas.OpenAI)
	require.NoError(t, err)
	require.NotNil(t, cfg)
}

func TestScopeProviderByRuntimeKey_CustomProviderUsesTypeAndName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	customType := tables.CustomProviderPicklistItemID
	provider := tables.TableProvider{
		Name:         "my-custom-gateway",
		ProviderType: &customType,
		CustomProviderConfigJSON: `{"base_provider_type":"openai","is_key_less":true}`,
	}
	require.NoError(t, store.DB().Create(&provider).Error)

	cfg, err := store.GetProviderConfig(ctx, schemas.ModelProvider("my-custom-gateway"))
	require.NoError(t, err)
	require.NotNil(t, cfg)
}
