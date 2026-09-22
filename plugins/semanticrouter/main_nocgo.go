//go:build !cgo

package semanticrouter

import (
	"context"
	"errors"

	"github.com/maximhq/bifrost/core/schemas"
)

var ErrNativeRuntimeUnavailable = errors.New("semantic-router plugin requires CGO_ENABLED=1 and built native libraries")

type Plugin struct{}

func Init(*Config, schemas.Logger) (*Plugin, error) {
	return nil, ErrNativeRuntimeUnavailable
}

func (*Plugin) GetName() string {
	return PluginName
}

func (*Plugin) Cleanup() error {
	return nil
}

func (*Plugin) Preview(context.Context, PreviewInput) (*PreviewResult, error) {
	return nil, ErrNativeRuntimeUnavailable
}

var _ Router = (*Plugin)(nil)
