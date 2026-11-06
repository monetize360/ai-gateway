package guardrails

import (
	"context"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

const parallelInputKey = "guardrail-parallel-input"

// inputCheck is an input evaluation that runs while the provider call is in flight.
type inputCheck struct {
	done     chan struct{}
	result   StageResult
	mu       sync.Mutex
	recorded bool
	emitted  bool
}

func (p *Plugin) startParallelInput(ctx *schemas.BifrostContext, policies []Policy, messages []MessageText) {
	check := &inputCheck{done: make(chan struct{})}
	ctx.SetValue(parallelInputKey, check)
	checkCtx := withCallerToken(context.Background(), callerToken(ctx))
	go func() {
		defer close(check.done)
		check.result = p.evaluateStage(checkCtx, policies, messages, "input")
	}()
}

// consumeParallelInput waits for the background input check.
// The first block is the HTTP 400. Later stream chunks are skipped.
func (p *Plugin) consumeParallelInput(ctx *schemas.BifrostContext) *schemas.BifrostError {
	check, _ := ctx.Value(parallelInputKey).(*inputCheck)
	if check == nil {
		return nil
	}
	<-check.done
	check.mu.Lock()
	defer check.mu.Unlock()
	if !check.recorded {
		p.recordResult(ctx, check.result)
		check.recorded = true
	}
	if check.result.Decision != DecisionBlock {
		return nil
	}
	if check.emitted {
		skip := true
		logError := false
		return &schemas.BifrostError{StreamControl: &schemas.StreamControl{SkipStream: &skip, LogError: &logError}}
	}
	check.emitted = true
	return blockError(blockMessage(check.result))
}
