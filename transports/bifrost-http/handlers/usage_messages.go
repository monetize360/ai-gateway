package handlers

import (
	"errors"
	"time"

	"github.com/valyala/fasthttp"
)

var errInvalidDateRange = errors.New("invalid date range")

// logTimeRange reads start_time and end_time. A period that resolves replaces
// both bounds. An unparseable bound, or a start after the end, is rejected.
func logTimeRange(ctx *fasthttp.RequestCtx) (start, end *time.Time, err error) {
	if period := string(ctx.QueryArgs().Peek("period")); period != "" {
		if periodStart, periodEnd := ResolvePeriod(period); periodStart != nil {
			return periodStart, periodEnd, nil
		}
	}

	startRaw := string(ctx.QueryArgs().Peek("start_time"))
	endRaw := string(ctx.QueryArgs().Peek("end_time"))
	if startRaw != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, startRaw)
		if parseErr != nil {
			if logger != nil {
				logger.Warn("rejected start_time %q: %v", startRaw, parseErr)
			}
			return nil, nil, errInvalidDateRange
		}
		start = &parsed
	}
	if endRaw != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, endRaw)
		if parseErr != nil {
			if logger != nil {
				logger.Warn("rejected end_time %q: %v", endRaw, parseErr)
			}
			return nil, nil, errInvalidDateRange
		}
		end = &parsed
	}
	if start != nil && end != nil && start.After(*end) {
		return nil, nil, errInvalidDateRange
	}
	return start, end, nil
}
