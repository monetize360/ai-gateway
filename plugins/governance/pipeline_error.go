package governance

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/configstore"
)

const (
	// systemUserID is RoleConstants.SYSTEM_USER_ID in MPilot.
	systemUserID = "5cf0f7e7-ed82-4bf2-ba51-8e834e4a3717"
	// gatewayPublishStageID is BillingPipelineErrorStage.GATEWAY_PUBLISH.
	gatewayPublishStageID = "a7c4e1b8-9d26-4f53-8c10-6e2b5a9d3f74"

	billingPipelineErrorTable = "billing_pipeline_error__m"
	pipelineErrorStatusFailed = "FAILED"

	pipelineErrorTypePublishFailed = "PublishFailed"

	maxPipelineErrorMessageLength = 4000
	maxPipelineErrorPayloadChars  = 16384
)

// recordBillingPipelineError inserts a gateway usage-publish failure directly
// into the tenant BillingPipelineError table. Recording is best-effort: a
// write failure is logged and never returned to the caller.
func (p *GovernancePlugin) recordBillingPipelineError(
	ctx context.Context,
	tenantID, errorType, errorMessage, externalTransactionID string,
	payload map[string]any,
) {
	if p == nil || p.registry == nil || strings.TrimSpace(tenantID) == "" {
		return
	}
	writeCtx := context.WithoutCancel(ctx)
	store := p.registry.GetStoreForTenant(writeCtx, tenantID)
	rdb, ok := store.(*configstore.RDBConfigStore)
	if !ok || rdb == nil || rdb.DB() == nil {
		if p.logger != nil {
			p.logger.Error("billing pipeline error not recorded; tenant database unavailable (tenant=%s type=%s)", tenantID, errorType)
		}
		return
	}

	payloadJSON := pipelineErrorPayload(payload)
	var externalID any
	if trimmed := strings.TrimSpace(externalTransactionID); trimmed != "" {
		externalID = trimmed
	}

	err := rdb.DB().WithContext(writeCtx).Exec(`
		INSERT INTO `+billingPipelineErrorTable+` (
			id, deleted, created_at, updated_at, created_by, updated_by,
			stage, error_type, error_message, external_transaction_id, payload, status
		) VALUES (
			?, FALSE, NOW(), NOW(), ?, ?,
			?, ?, ?, ?, CAST(? AS jsonb), ?
		)`,
		uuid.NewString(),
		systemUserID,
		systemUserID,
		gatewayPublishStageID,
		errorType,
		truncatePipelineError(errorMessage, maxPipelineErrorMessageLength),
		externalID,
		payloadJSON,
		pipelineErrorStatusFailed,
	).Error
	if err != nil && p.logger != nil {
		p.logger.Error("failed to record billing pipeline error (tenant=%s type=%s): %v", tenantID, errorType, err)
	}
}

func pipelineErrorPayload(payload map[string]any) string {
	if len(payload) == 0 {
		return `{"raw":""}`
	}
	encoded, err := sonic.Marshal(payload)
	if err != nil {
		return `{"raw":""}`
	}
	if len(encoded) <= maxPipelineErrorPayloadChars {
		return string(encoded)
	}
	capped, err := sonic.Marshal(map[string]string{
		"raw": truncatePipelineError(string(encoded), maxPipelineErrorPayloadChars),
	})
	if err != nil {
		return `{"raw":""}`
	}
	return string(capped)
}

func truncatePipelineError(value string, maxLength int) string {
	if len(value) <= maxLength {
		return value
	}
	return value[:maxLength]
}
