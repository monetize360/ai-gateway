package guardrails

import (
	"strings"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

type streamBuf struct {
	mu        sync.Mutex
	text      strings.Builder
	since     int
	checked   bool
	blocked   bool
	mode      string
	chunkSize int
}

func (p *Plugin) streamBuffer(ctx *schemas.BifrostContext, policies []Policy) *streamBuf {
	if existing, ok := ctx.Value(streamStateKey).(*streamBuf); ok && existing != nil {
		return existing
	}
	mode := tablesStreamChunked
	chunkSize := 240
	for _, policy := range policies {
		if policy.StreamMode == "buffered" {
			mode = "buffered"
		}
		if policy.StreamChunkChars > 0 {
			chunkSize = policy.StreamChunkChars
		}
	}
	buf := &streamBuf{mode: mode, chunkSize: chunkSize}
	ctx.SetValue(streamStateKey, buf)
	return buf
}

const tablesStreamChunked = "chunked"

func (p *Plugin) onStreamChunk(ctx *schemas.BifrostContext, chunk *schemas.BifrostStreamChunk, policies []Policy) (*schemas.BifrostStreamChunk, error) {
	if chunk == nil || chunk.BifrostError != nil || len(policies) == 0 {
		return chunk, nil
	}
	buf := p.streamBuffer(ctx, policies)
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if buf.blocked {
		return nil, errStreamStopped
	}
	delta, contentPtr, finished := chunkDelta(chunk)
	if delta != "" {
		buf.text.WriteString(delta)
		buf.since += len(delta)
	}
	shouldCheck := finished || (buf.mode == tablesStreamChunked && buf.since >= buf.chunkSize && buf.since > 0)
	if buf.mode == "buffered" && !finished && contentPtr != nil {
		empty := ""
		*contentPtr = &empty
		return chunk, nil
	}
	if !shouldCheck {
		return chunk, nil
	}
	buf.since = 0
	messages := []MessageText{{Role: "assistant", Text: buf.text.String()}}
	result := p.evaluateStage(ctx, policies, messages, "output")
	p.recordResult(ctx, result)
	if result.Decision == DecisionBlock {
		buf.blocked = true
		return blockChunk(blockMessage(result)), nil
	}
	if result.Decision == DecisionRedact && buf.mode == "buffered" && finished && contentPtr != nil {
		*contentPtr = &result.OutputText
	}
	return chunk, nil
}
