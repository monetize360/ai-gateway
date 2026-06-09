package configstore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsurePostgresDSNUTC(t *testing.T) {
	require.Equal(t, "", ensurePostgresDSNUTC(""))
	require.Equal(t, "host=localhost dbname=test timezone=UTC", ensurePostgresDSNUTC("host=localhost dbname=test"))
	require.Equal(t, "host=localhost timezone=UTC", ensurePostgresDSNUTC("host=localhost timezone=UTC"))
	require.Equal(t, "host=localhost TimeZone=Europe/Berlin", ensurePostgresDSNUTC("host=localhost TimeZone=Europe/Berlin"))
}
