package tenantstore

import (
	"crypto/rsa"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// TenantClaims mirrors the JWT payload issued by the mpilotv2 backend.
// Field names use the camelCase JSON tags that the backend embeds in the token.
type TenantClaims struct {
	// TenantID is the UUID of the tenant the request belongs to.
	TenantID string `json:"tenantId"`
	// UserID is the UUID of the authenticated user.
	UserID string `json:"userId"`
	// MorgID is the UUID of the user's organisation within the tenant.
	MorgID string `json:"morgId"`
	// VirtualKey is the UUID primary key of the governance_virtual_keys row
	// representing this credential. It is used to validate the key against the
	// tenant DB and to drive governance data lookup.
	VirtualKey string `json:"virtualKey"`
	// Roles holds the authority strings assigned to the user (e.g. "TENANTADMIN").
	Roles []string `json:"roles"`

	jwt.RegisteredClaims
}

// ExtractClaimsFromJWT validates a JWT signed by the mpilotv2 backend and
// returns the parsed TenantClaims. The signing key is the shared secret stored
// in config.json and applies to every tenant.
//
// Supported algorithms (auto-detected from the token header):
//   - RS256 / RS384 / RS512 — key must be a PEM-encoded RSA public key.
//   - HS256 / HS384 / HS512 — key is used directly as an HMAC secret.
//
// Returns an error when:
//   - the token is empty or malformed
//   - the signing method is unexpected
//   - the signature is invalid
//   - the token has expired or is not yet valid
//   - tenantId or virtualKey claims are missing
func ExtractClaimsFromJWT(tokenStr string, key []byte) (*TenantClaims, error) {
	return extractClaimsFromJWTWithKeys(tokenStr, true, key)
}

// ExtractTenantAuthClaimsFromJWT validates a JWT signed with any of the supplied keys
// and returns claims when tenantId is present. virtualKey is optional (MPilot user tokens).
func ExtractTenantAuthClaimsFromJWT(tokenStr string, keys ...[]byte) (*TenantClaims, error) {
	return extractClaimsFromJWTWithKeys(tokenStr, false, keys...)
}

func extractClaimsFromJWTWithKeys(tokenStr string, requireVirtualKey bool, keys ...[]byte) (*TenantClaims, error) {
	if tokenStr == "" {
		return nil, fmt.Errorf("token string is empty")
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWT key must not be empty")
	}

	var lastErr error
	for _, key := range keys {
		if len(key) == 0 {
			continue
		}
		claims, err := parseTenantClaims(tokenStr, key)
		if err != nil {
			lastErr = err
			continue
		}
		if claims.TenantID == "" {
			return nil, fmt.Errorf("token missing tenantId claim")
		}
		if requireVirtualKey && claims.VirtualKey == "" {
			return nil, fmt.Errorf("token missing virtualKey claim")
		}
		return claims, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("JWT key must not be empty")
}

func parseTenantClaims(tokenStr string, key []byte) (*TenantClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TenantClaims{}, func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodRSA:
			pubKey, parseErr := jwt.ParseRSAPublicKeyFromPEM(key)
			if parseErr != nil {
				return nil, fmt.Errorf("invalid RSA public key in config: %w", parseErr)
			}
			return pubKey, nil
		case *jwt.SigningMethodHMAC:
			return key, nil
		default:
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("token has expired")
		}
		if errors.Is(err, jwt.ErrTokenNotValidYet) {
			return nil, fmt.Errorf("token is not yet valid")
		}
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*TenantClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("could not parse token claims")
	}
	return claims, nil
}

// ExtractTenantIDFromJWT is a backward-compatible shim that calls
// ExtractClaimsFromJWT and returns only the tenantId.
// Prefer ExtractClaimsFromJWT when you also need the virtualKey or other claims.
func ExtractTenantIDFromJWT(tokenStr string, key []byte) (string, error) {
	claims, err := ExtractClaimsFromJWT(tokenStr, key)
	if err != nil {
		return "", err
	}
	return claims.TenantID, nil
}

// ParseRSAPublicKey is a helper that parses a PEM-encoded RSA public key.
// Exposed so server bootstrap code can pre-validate the configured key on startup.
func ParseRSAPublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	k, err := jwt.ParseRSAPublicKeyFromPEM(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse RSA public key: %w", err)
	}
	return k, nil
}
