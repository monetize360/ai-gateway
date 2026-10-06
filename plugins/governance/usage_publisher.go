package governance

import "context"

// UsageEventPublisher publishes InferenceUsage and billing-alert events for MPilot.
// Implemented in the HTTP transport with kafkainject.Pool — do not import transports here.
type UsageEventPublisher interface {
	PublishUsage(ctx context.Context, tenantID, key string, message map[string]any) error
	PublishAlert(ctx context.Context, tenantID, key string, message map[string]any) error
}
