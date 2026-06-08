package handlers

import (
	"fmt"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

func requireConfigStore(cfg *lib.Config, ctx *fasthttp.RequestCtx) (configstore.ConfigStore, error) {
	store, err := cfg.RequireStoreFromRequestCtx(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant context required: %w", err)
	}
	return store, nil
}
