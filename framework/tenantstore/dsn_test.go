package tenantstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDbNameFromURL_JDBCFull(t *testing.T) {
	name, err := dbNameFromURL("jdbc:postgresql://localhost:5432/tenant2")
	require.NoError(t, err)
	assert.Equal(t, "tenant2", name)
}

func TestDbNameFromURL_JDBCNoPort(t *testing.T) {
	name, err := dbNameFromURL("jdbc:postgresql://db.example.com/mydb")
	require.NoError(t, err)
	assert.Equal(t, "mydb", name)
}

func TestDbNameFromURL_JDBCWithQuery(t *testing.T) {
	name, err := dbNameFromURL("jdbc:postgresql://localhost:5432/tenant3?sslmode=require&foo=bar")
	require.NoError(t, err)
	assert.Equal(t, "tenant3", name)
}

func TestDbNameFromURL_PlainPostgres(t *testing.T) {
	name, err := dbNameFromURL("postgresql://localhost/mydb")
	require.NoError(t, err)
	assert.Equal(t, "mydb", name)
}

func TestDbNameFromURL_Empty(t *testing.T) {
	_, err := dbNameFromURL("")
	assert.Error(t, err)
}

func TestDbNameFromURL_MissingDBName(t *testing.T) {
	_, err := dbNameFromURL("jdbc:postgresql://localhost/")
	assert.Error(t, err)
}

func TestFormatJWTVerificationKey_RawBase64(t *testing.T) {
	raw := "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA"
	pem := FormatJWTVerificationKey(raw)
	assert.Contains(t, string(pem), "BEGIN PUBLIC KEY")
	assert.Contains(t, string(pem), raw)
}

func TestFormatJWTVerificationKey_AlreadyPEM(t *testing.T) {
	pemIn := "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----"
	assert.Equal(t, []byte(pemIn), FormatJWTVerificationKey(pemIn))
}
