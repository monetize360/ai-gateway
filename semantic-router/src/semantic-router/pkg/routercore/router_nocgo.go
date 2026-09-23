//go:build !cgo

package routercore

import (
	"context"
	"errors"
)

var ErrNativeRuntimeUnavailable = errors.New("embedded semantic router requires CGO_ENABLED=1 and built native libraries")

type Options struct{ Entrypoint string }

type Core struct{}

func NewFromFile(context.Context, string, Options) (*Core, error) {
	return nil, ErrNativeRuntimeUnavailable
}

func (*Core) Close() error {
	return nil
}
