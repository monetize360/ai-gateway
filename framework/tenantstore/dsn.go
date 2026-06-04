package tenantstore

import "strings"

// FormatJWTVerificationKey normalises the key material from config.json.
// RS256 public keys may be supplied as raw base64 (DER) or a full PEM block;
// HMAC secrets are returned as-is.
func FormatJWTVerificationKey(keyMaterial string) []byte {
	keyMaterial = strings.TrimSpace(keyMaterial)
	if keyMaterial == "" {
		return nil
	}
	if strings.Contains(keyMaterial, "BEGIN PUBLIC KEY") || strings.Contains(keyMaterial, "BEGIN RSA PUBLIC KEY") {
		return []byte(keyMaterial)
	}
	// Raw base64 DER from config — wrap in PEM for jwt.ParseRSAPublicKeyFromPEM.
	const lineWidth = 64
	var b strings.Builder
	b.WriteString("-----BEGIN PUBLIC KEY-----\n")
	for i := 0; i < len(keyMaterial); i += lineWidth {
		end := i + lineWidth
		if end > len(keyMaterial) {
			end = len(keyMaterial)
		}
		b.WriteString(keyMaterial[i:end])
		b.WriteByte('\n')
	}
	b.WriteString("-----END PUBLIC KEY-----\n")
	return []byte(b.String())
}
