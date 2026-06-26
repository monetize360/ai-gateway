package routing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractParamKeysFromCEL(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want []string
	}{
		{
			name: "bracket access",
			expr: `params["Region"] == "us-east-1"`,
			want: []string{"region"},
		},
		{
			name: "in operator",
			expr: `"Env" in params`,
			want: []string{"env"},
		},
		{
			name: "combined",
			expr: `"region" in params && params["region"] == "us-east-1"`,
			want: []string{"region"},
		},
		{
			name: "multiple keys",
			expr: `params["region"] == "us-east-1" && params["env"] == "prod"`,
			want: []string{"region", "env"},
		},
		{
			name: "no params",
			expr: `model == "gpt-4o"`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractParamKeysFromCEL(tt.expr))
		})
	}
}
