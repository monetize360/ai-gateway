package governance

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// alertClaim is the in-memory record that this node already inserted or observed
// a notification for (threshold_id, period_key). It is not stored on the budget or wallet
// row, because those rows are replaced wholesale from the database.
type alertClaim struct {
	notificationID string
	published      bool
	owned          bool
}

// BillingAlertEvent is one claimed notification ready to publish. TenantId is added by the tracker.
type BillingAlertEvent struct {
	NotificationID string
	Message        map[string]any
}

func alertClaimKey(thresholdID, periodKey string) string {
	return thresholdID + "\x00" + periodKey
}

func budgetAlertPeriodKey(usage *configstoreTables.TableBudgetUsage) string {
	if usage == nil {
		return "created"
	}
	if usage.LastReset != nil && !usage.LastReset.IsZero() {
		return strconv.FormatInt(usage.LastReset.UTC().UnixMicro(), 10)
	}
	if !usage.CreatedAt.IsZero() {
		return strconv.FormatInt(usage.CreatedAt.UTC().UnixMicro(), 10)
	}
	return "created"
}

func indexAlertThresholds(byID map[string]*configstoreTables.TableAlertThreshold) map[string][]*configstoreTables.TableAlertThreshold {
	byTarget := make(map[string][]*configstoreTables.TableAlertThreshold, len(byID))
	for _, row := range byID {
		if row == nil || row.TargetID == "" {
			continue
		}
		byTarget[row.TargetID] = append(byTarget[row.TargetID], row)
	}
	return byTarget
}

// replaceAlertCache loads thresholds and existing notifications. A published notification is done.
// An unpublished notification stays claimed so the next scan publishes that same row.
func (gs *LocalGovernanceStore) replaceAlertCache(thresholds []configstoreTables.TableAlertThreshold, notifications []configstoreTables.TableAlertNotification) {
	byID := make(map[string]*configstoreTables.TableAlertThreshold, len(thresholds))
	for i := range thresholds {
		row := thresholds[i]
		if row.Deleted || row.ID == "" {
			continue
		}
		clone := row
		byID[row.ID] = &clone
	}
	claims := make(map[string]*alertClaim, len(notifications))
	for i := range notifications {
		row := notifications[i]
		if row.Deleted || row.ThresholdID == "" || row.PeriodKey == "" {
			continue
		}
		claims[alertClaimKey(row.ThresholdID, row.PeriodKey)] = &alertClaim{
			notificationID: row.ID,
			published:      row.Published,
			owned:          !row.Published,
		}
	}
	gs.alertMu.Lock()
	gs.alertThresholdsByID = byID
	gs.alertThresholdsByTarget = indexAlertThresholds(byID)
	gs.alertClaims = claims
	gs.alertMu.Unlock()
}

func (gs *LocalGovernanceStore) applyAlertThresholdDelta(rows []configstoreTables.TableAlertThreshold) {
	gs.alertMu.Lock()
	defer gs.alertMu.Unlock()
	if gs.alertThresholdsByID == nil {
		gs.alertThresholdsByID = map[string]*configstoreTables.TableAlertThreshold{}
	}
	for i := range rows {
		row := rows[i]
		if row.ID == "" {
			continue
		}
		if row.Deleted {
			delete(gs.alertThresholdsByID, row.ID)
			continue
		}
		clone := row
		gs.alertThresholdsByID[row.ID] = &clone
	}
	gs.alertThresholdsByTarget = indexAlertThresholds(gs.alertThresholdsByID)
}

// ScanAlertThresholds compares enabled thresholds to the cached budget and wallet rows.
// The database is touched only when a line is newly crossed, a wallet breach clears, or a claim is read back.
func (gs *LocalGovernanceStore) ScanAlertThresholds(ctx context.Context) []BillingAlertEvent {
	if gs == nil || gs.configStore == nil {
		return nil
	}
	gs.alertMu.Lock()
	defer gs.alertMu.Unlock()
	if gs.alertClaims == nil {
		gs.alertClaims = map[string]*alertClaim{}
	}
	if len(gs.alertThresholdsByTarget) == 0 {
		return nil
	}

	var events []BillingAlertEvent
	for _, thresholds := range gs.alertThresholdsByTarget {
		for _, threshold := range thresholds {
			event, ok := gs.evaluateThresholdLocked(ctx, threshold)
			if ok {
				events = append(events, event)
			}
		}
	}
	return events
}

// MarkAlertNotificationPublished records that Kafka accepted this notification so later ticks do not publish it again.
func (gs *LocalGovernanceStore) MarkAlertNotificationPublished(ctx context.Context, notificationID string) error {
	if gs == nil || gs.configStore == nil || notificationID == "" {
		return nil
	}
	if err := gs.configStore.MarkAlertNotificationPublished(ctx, notificationID); err != nil {
		return err
	}
	gs.alertMu.Lock()
	defer gs.alertMu.Unlock()
	for _, claim := range gs.alertClaims {
		if claim != nil && claim.notificationID == notificationID {
			claim.published = true
		}
	}
	return nil
}

func (gs *LocalGovernanceStore) evaluateThresholdLocked(ctx context.Context, threshold *configstoreTables.TableAlertThreshold) (BillingAlertEvent, bool) {
	if !configstoreTables.AlertThresholdEnabled(threshold) || strings.TrimSpace(threshold.TargetID) == "" {
		return BillingAlertEvent{}, false
	}
	switch threshold.TargetType {
	case configstoreTables.AlertTargetBudgetUsage:
		if threshold.Metric != configstoreTables.AlertMetricUsagePercent &&
			threshold.Metric != configstoreTables.AlertMetricUsageAmount {
			return BillingAlertEvent{}, false
		}
		return gs.evaluateBudgetThresholdLocked(ctx, threshold)
	case configstoreTables.AlertTargetWallet:
		if threshold.Metric != configstoreTables.AlertMetricAvailableBalance {
			return BillingAlertEvent{}, false
		}
		return gs.evaluateWalletThresholdLocked(ctx, threshold)
	default:
		return BillingAlertEvent{}, false
	}
}

func (gs *LocalGovernanceStore) evaluateBudgetThresholdLocked(ctx context.Context, threshold *configstoreTables.TableAlertThreshold) (BillingAlertEvent, bool) {
	raw, ok := gs.budgetUsages.Load(threshold.TargetID)
	if !ok || raw == nil {
		return BillingAlertEvent{}, false
	}
	usage, ok := raw.(*configstoreTables.TableBudgetUsage)
	if !ok || usage == nil || usage.Deleted || usage.MaxLimit <= 0 {
		return BillingAlertEvent{}, false
	}
	// The reset worker owns an elapsed window. Skip it so this minute scan cannot
	// alert on spend that is about to be zeroed and moved to the next period.
	if isBudgetUsagePeriodExpired(usage, time.Now()) {
		return BillingAlertEvent{}, false
	}
	periodKey := budgetAlertPeriodKey(usage)
	usageAmount := usage.CurrentUsage
	organizationID := gs.organizationIDForAccount(usage.AccountID)
	resetsAt := budgetUsageResetsAt(usage)
	if threshold.Metric == configstoreTables.AlertMetricUsageAmount {
		if usage.CurrentUsage < threshold.ThresholdValue {
			return BillingAlertEvent{}, false
		}
		return gs.claimThresholdLocked(ctx, threshold, periodKey, usageAmount, usage.MaxLimit, organizationID, &usageAmount, resetsAt)
	}
	percent := usage.CurrentUsage / usage.MaxLimit * 100
	if percent < threshold.ThresholdValue {
		return BillingAlertEvent{}, false
	}
	return gs.claimThresholdLocked(ctx, threshold, periodKey, percent, usage.MaxLimit, organizationID, &usageAmount, resetsAt)
}

func (gs *LocalGovernanceStore) evaluateWalletThresholdLocked(ctx context.Context, threshold *configstoreTables.TableAlertThreshold) (BillingAlertEvent, bool) {
	raw, ok := gs.wallets.Load(threshold.TargetID)
	if !ok || raw == nil {
		return BillingAlertEvent{}, false
	}
	wallet, ok := raw.(*configstoreTables.TableWallet)
	if !ok || wallet == nil || wallet.Deleted {
		return BillingAlertEvent{}, false
	}
	balance := walletFunds(wallet)
	periodKey := configstoreTables.WalletAlertPeriodKey
	if balance >= threshold.ThresholdValue {
		gs.rearmWalletLocked(ctx, threshold.ID, periodKey)
		return BillingAlertEvent{}, false
	}
	return gs.claimThresholdLocked(ctx, threshold, periodKey, balance, threshold.ThresholdValue, gs.organizationIDForAccount(wallet.AccountID), nil, "")
}

func (gs *LocalGovernanceStore) claimThresholdLocked(ctx context.Context, threshold *configstoreTables.TableAlertThreshold, periodKey string, observed, limit float64, organizationID string, usageAmount *float64, resetsAt string) (BillingAlertEvent, bool) {
	key := alertClaimKey(threshold.ID, periodKey)
	if claim := gs.alertClaims[key]; claim != nil {
		if claim.published || !claim.owned || claim.notificationID == "" {
			return BillingAlertEvent{}, false
		}
		return billingAlertEvent(threshold, claim.notificationID, periodKey, observed, limit, organizationID, usageAmount, resetsAt), true
	}

	now := time.Now().UTC()
	notificationID := uuid.NewString()
	inserted, err := gs.configStore.InsertAlertNotification(ctx, &configstoreTables.TableAlertNotification{
		ID:            notificationID,
		ThresholdID:   threshold.ID,
		PeriodKey:     periodKey,
		ObservedValue: observed,
		FiredAt:       now,
		Published:     false,
		Notified:      false,
		SystemColumns: configstoreTables.SystemColumns{
			CreatedAt: now,
			UpdatedAt: now,
		},
	})
	if err != nil {
		if gs.logger != nil {
			gs.logger.Error("failed to claim alert threshold %s: %v", threshold.ID, err)
		}
		return BillingAlertEvent{}, false
	}
	if !inserted {
		// Another node owns this key. Remember it and leave the publish to that node.
		gs.alertClaims[key] = &alertClaim{published: true, owned: false}
		return BillingAlertEvent{}, false
	}
	gs.alertClaims[key] = &alertClaim{notificationID: notificationID, published: false, owned: true}
	return billingAlertEvent(threshold, notificationID, periodKey, observed, limit, organizationID, usageAmount, resetsAt), true
}

func (gs *LocalGovernanceStore) rearmWalletLocked(ctx context.Context, thresholdID, periodKey string) {
	key := alertClaimKey(thresholdID, periodKey)
	if gs.alertClaims[key] == nil {
		return
	}
	if err := gs.configStore.DeleteAlertNotification(ctx, thresholdID, periodKey); err != nil {
		if gs.logger != nil {
			gs.logger.Error("failed to clear wallet alert %s: %v", thresholdID, err)
		}
		return
	}
	delete(gs.alertClaims, key)
}

func (gs *LocalGovernanceStore) organizationIDForAccount(accountID *string) string {
	if accountID == nil || strings.TrimSpace(*accountID) == "" {
		return ""
	}
	raw, ok := gs.accounts.Load(strings.TrimSpace(*accountID))
	if !ok || raw == nil {
		return ""
	}
	account, ok := raw.(*configstoreTables.TableAccount)
	if !ok || account == nil || account.CustomerOrganizationID == nil {
		return ""
	}
	return strings.TrimSpace(*account.CustomerOrganizationID)
}

func alertRecipientIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

func budgetUsageResetsAt(usage *configstoreTables.TableBudgetUsage) string {
	if usage == nil || usage.ResetDuration == "" {
		return ""
	}
	duration, err := configstoreTables.ParseDuration(usage.ResetDuration)
	if err != nil {
		return ""
	}
	periodStart := budgetUsagePeriodStart(usage)
	if periodStart.IsZero() {
		return ""
	}
	return periodStart.UTC().Add(duration).Format(time.RFC3339)
}

func billingAlertEvent(threshold *configstoreTables.TableAlertThreshold, notificationID, periodKey string, observed, limit float64, organizationID string, usageAmount *float64, resetsAt string) BillingAlertEvent {
	message := map[string]any{
		"notificationId": notificationID,
		"thresholdId":    threshold.ID,
		"targetType":     threshold.TargetType,
		"targetId":       threshold.TargetID,
		"metric":         threshold.Metric,
		"observedValue":  observed,
		"limit":          limit,
		"thresholdValue": threshold.ThresholdValue,
		"periodKey":      periodKey,
	}
	if name := strings.TrimSpace(threshold.Name); name != "" {
		message["name"] = name
	}
	if organizationID != "" {
		message["organizationId"] = organizationID
	}
	if roleIDs := alertRecipientIDs(threshold.RecipientRoleIDs); len(roleIDs) > 0 {
		message["recipientRoleIds"] = roleIDs
	}
	if userIDs := alertRecipientIDs(threshold.RecipientUserIDs); len(userIDs) > 0 {
		message["recipientUserIds"] = userIDs
	}
	if usageAmount != nil {
		message["usageAmount"] = *usageAmount
	}
	if resetsAt != "" {
		message["resetsAt"] = resetsAt
	}
	return BillingAlertEvent{NotificationID: notificationID, Message: message}
}
