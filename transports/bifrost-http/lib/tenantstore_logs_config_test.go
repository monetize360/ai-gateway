package lib

import (
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeLogsStorePostgresFromTenantStore_InheritsGlobal(t *testing.T) {
	tenantStore := &TenantStoreFileConfig{
		MaxIdleConns: 4,
		MaxOpenConns: 8,
		Global: &TenantStoreGlobalPostgresFile{
			Host:         "localhost",
			Port:         "5432",
			User:         "postgres",
			Password:     "secret",
			DBName:       "mpilotv2",
			SSLMode:      "disable",
			MaxIdleConns: 2,
			MaxOpenConns: 10,
		},
	}

	pg := &logstore.PostgresConfig{}
	require.NoError(t, MergeLogsStorePostgresFromTenantStore(pg, tenantStore))

	assert.Equal(t, "localhost", pg.Host.GetValue())
	assert.Equal(t, "5432", pg.Port.GetValue())
	assert.Equal(t, "postgres", pg.User.GetValue())
	assert.Equal(t, "secret", pg.Password.GetValue())
	assert.Equal(t, "mpilotv2", pg.DBName.GetValue())
	assert.Equal(t, "disable", pg.SSLMode.GetValue())
	assert.Equal(t, 2, pg.MaxIdleConns)
	assert.Equal(t, 10, pg.MaxOpenConns)
}

func TestPrepareLogsStoreConfig_OmittedPostgresConfig(t *testing.T) {
	raw := `{
		"tenant_store": {
			"enabled": true,
			"global": {
				"host": "db.example",
				"port": "5432",
				"user": "postgres",
				"password": "secret",
				"db_name": "mpilotv2",
				"ssl_mode": "disable"
			}
		},
		"logs_store": {
			"enabled": true,
			"type": "postgres"
		}
	}`

	var configData ConfigData
	require.NoError(t, json.Unmarshal([]byte(raw), &configData))
	require.NoError(t, prepareLogsStoreConfig(&configData))

	pg, ok := configData.LogsStoreConfig.Config.(*logstore.PostgresConfig)
	require.True(t, ok)
	assert.Equal(t, "db.example", pg.Host.GetValue())
	assert.Equal(t, "mpilotv2", pg.DBName.GetValue())
}
