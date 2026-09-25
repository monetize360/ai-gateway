//go:build !cgo

package semanticrouter

import (
	"errors"
	"testing"
)

func TestInitWithoutCGOReturnsActionableCapabilityError(t *testing.T) {
	_, err := Init(&Config{RecipeFile: "unused.yaml"}, nil)
	if !errors.Is(err, ErrNativeRuntimeUnavailable) {
		t.Fatalf("Init() error = %v, want %v", err, ErrNativeRuntimeUnavailable)
	}
}
