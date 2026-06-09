package tenantstore

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSecret = []byte("super-secret-key-for-testing-only")

// makeHMACToken builds a signed HS256 JWT with the full TenantClaims payload.
func makeHMACToken(tenantID, virtualKey, userID string, expiresIn time.Duration) string {
	claims := TenantClaims{
		TenantID:   tenantID,
		VirtualKey: virtualKey,
		UserID:     userID,
		MorgID:    "morg-123",
		Roles:     []string{"TENANTADMIN"},
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiresIn)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString(testSecret)
	return signed
}

// ── ExtractClaimsFromJWT ──────────────────────────────────────────────────────

func TestExtractClaimsFromJWT_Valid(t *testing.T) {
	tenantID := "550e8400-e29b-41d4-a716-446655440000"
	virtualKey := "af7a4c0d-d719-49fe-b004-57c0b32b17a4"
	token := makeHMACToken(tenantID, virtualKey, "user-1", time.Hour)

	claims, err := ExtractClaimsFromJWT(token, testSecret)
	require.NoError(t, err)
	assert.Equal(t, tenantID, claims.TenantID)
	assert.Equal(t, virtualKey, claims.VirtualKey)
	assert.Equal(t, "user-1", claims.UserID)
	assert.Equal(t, "morg-123", claims.MorgID)
	assert.Equal(t, []string{"TENANTADMIN"}, claims.Roles)
}

func TestExtractClaimsFromJWT_WrongSecret(t *testing.T) {
	token := makeHMACToken("t1", "k1", "u1", time.Hour)
	_, err := ExtractClaimsFromJWT(token, []byte("wrong-secret"))
	assert.Error(t, err)
}

func TestExtractClaimsFromJWT_Expired(t *testing.T) {
	token := makeHMACToken("t1", "k1", "u1", -time.Minute)
	_, err := ExtractClaimsFromJWT(token, testSecret)
	assert.ErrorContains(t, err, "expired")
}

func TestExtractClaimsFromJWT_MissingTenantID(t *testing.T) {
	claims := TenantClaims{
		VirtualKey: "key-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString(testSecret)

	_, err := ExtractClaimsFromJWT(signed, testSecret)
	assert.ErrorContains(t, err, "tenantId")
}

func TestExtractClaimsFromJWT_MissingVirtualKey(t *testing.T) {
	claims := TenantClaims{
		TenantID: "tenant-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString(testSecret)

	_, err := ExtractClaimsFromJWT(signed, testSecret)
	assert.ErrorContains(t, err, "virtualKey")
}

func TestExtractClaimsFromJWT_EmptyToken(t *testing.T) {
	_, err := ExtractClaimsFromJWT("", testSecret)
	assert.Error(t, err)
}

func TestExtractClaimsFromJWT_EmptyKey(t *testing.T) {
	token := makeHMACToken("t1", "k1", "u1", time.Hour)
	_, err := ExtractClaimsFromJWT(token, []byte{})
	assert.Error(t, err)
}

func TestExtractClaimsFromJWT_Malformed(t *testing.T) {
	_, err := ExtractClaimsFromJWT("not.a.jwt", testSecret)
	assert.Error(t, err)
}

// ── ExtractTenantIDFromJWT (backward-compat shim) ────────────────────────────

func TestExtractTenantIDFromJWT_Valid(t *testing.T) {
	tenantID := "550e8400-e29b-41d4-a716-446655440000"
	token := makeHMACToken(tenantID, "key-1", "user-1", time.Hour)

	got, err := ExtractTenantIDFromJWT(token, testSecret)
	require.NoError(t, err)
	assert.Equal(t, tenantID, got)
}
