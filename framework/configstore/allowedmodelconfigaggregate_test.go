package configstore

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

func ptr(s string) *string { return &s }

// makeRow is a helper to build a raw TableAllowedModelConfig row for tests.
func makeRow(vkID, providerID string, allowedModelID, blacklistedModelID *string, allowedName, blacklistedName string) tables.TableAllowedModelConfig {
	row := tables.TableAllowedModelConfig{
		ID:                 "row-" + vkID + "-" + providerID,
		VirtualKeyID:       ptr(vkID),
		ProviderID:         providerID,
		AllowedModelID:     allowedModelID,
		BlacklistedModelID: blacklistedModelID,
	}
	if allowedModelID != nil && allowedName != "" {
		row.AllowedModel = &tables.TableModel{ID: *allowedModelID, Name: allowedName}
	}
	if blacklistedModelID != nil && blacklistedName != "" {
		row.BlacklistedModel = &tables.TableModel{ID: *blacklistedModelID, Name: blacklistedName}
	}
	return row
}

func TestAggregateAllowedModelConfigs_NoRows(t *testing.T) {
	result := AggregateAllowedModelConfigs(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestAggregateAllowedModelConfigs_EmptySlice(t *testing.T) {
	result := AggregateAllowedModelConfigs([]tables.TableAllowedModelConfig{})
	if result != nil {
		t.Errorf("expected nil for empty slice, got %v", result)
	}
}

// TestAggregateAllowedModelConfigs_HeaderRowOnly — a single row with no model refs
// should yield AllowedModels ["*"] (allow-all-except-blacklist semantics).
func TestAggregateAllowedModelConfigs_HeaderRowOnly(t *testing.T) {
	header := tables.TableAllowedModelConfig{
		ID:           "h1",
		VirtualKeyID: ptr("vk1"),
		ProviderID:   "p1",
	}
	result := AggregateAllowedModelConfigs([]tables.TableAllowedModelConfig{header})
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "*" {
		t.Errorf("expected AllowedModels=[\"*\"], got %v", got.AllowedModels)
	}
	if len(got.BlacklistedModels) != 0 {
		t.Errorf("expected empty BlacklistedModels, got %v", got.BlacklistedModels)
	}
}

// TestAggregateAllowedModelConfigs_AllowOnlyRows — two allowed-model rows, no header.
func TestAggregateAllowedModelConfigs_AllowOnlyRows(t *testing.T) {
	rows := []tables.TableAllowedModelConfig{
		makeRow("vk1", "p1", ptr("m1"), nil, "gpt-4", ""),
		makeRow("vk1", "p1", ptr("m2"), nil, "gpt-4o", ""),
	}
	result := AggregateAllowedModelConfigs(rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	if len(got.AllowedModels) != 2 {
		t.Errorf("expected 2 allowed models, got %v", got.AllowedModels)
	}
	// Verify model names appear
	found := make(map[string]bool)
	for _, m := range got.AllowedModels {
		found[m] = true
	}
	if !found["gpt-4"] || !found["gpt-4o"] {
		t.Errorf("missing expected model names in AllowedModels: %v", got.AllowedModels)
	}
	if len(got.BlacklistedModels) != 0 {
		t.Errorf("expected no blacklisted models, got %v", got.BlacklistedModels)
	}
}

// TestAggregateAllowedModelConfigs_BlacklistOnlyRows — blacklisted rows with no allowed rows
// should yield AllowedModels ["*"] (allow all except blacklisted).
func TestAggregateAllowedModelConfigs_BlacklistOnlyRows(t *testing.T) {
	rows := []tables.TableAllowedModelConfig{
		makeRow("vk1", "p1", nil, ptr("m3"), "", "gpt-3"),
	}
	result := AggregateAllowedModelConfigs(rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "*" {
		t.Errorf("expected AllowedModels=[\"*\"], got %v", got.AllowedModels)
	}
	if len(got.BlacklistedModels) != 1 || got.BlacklistedModels[0] != "gpt-3" {
		t.Errorf("expected BlacklistedModels=[\"gpt-3\"], got %v", got.BlacklistedModels)
	}
}

// TestAggregateAllowedModelConfigs_MixedRows — allow + blacklist rows for same scope.
func TestAggregateAllowedModelConfigs_MixedRows(t *testing.T) {
	rows := []tables.TableAllowedModelConfig{
		makeRow("vk1", "p1", ptr("m1"), nil, "gpt-4", ""),
		makeRow("vk1", "p1", ptr("m2"), nil, "gpt-4o", ""),
		makeRow("vk1", "p1", nil, ptr("m3"), "", "gpt-3"),
	}
	result := AggregateAllowedModelConfigs(rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	if len(got.AllowedModels) != 2 {
		t.Errorf("expected 2 allowed models, got %v", got.AllowedModels)
	}
	if len(got.BlacklistedModels) != 1 || got.BlacklistedModels[0] != "gpt-3" {
		t.Errorf("expected BlacklistedModels=[\"gpt-3\"], got %v", got.BlacklistedModels)
	}
}

// TestAggregateAllowedModelConfigs_HeaderRowMetadata — weight/allow-all-keys come from header row.
func TestAggregateAllowedModelConfigs_HeaderRowMetadata(t *testing.T) {
	weight := float64(0.7)
	header := tables.TableAllowedModelConfig{
		ID:           "h1",
		VirtualKeyID: ptr("vk1"),
		ProviderID:   "p1",
		Weight:       &weight,
		AllowAllKeys: false,
	}
	modelRow := makeRow("vk1", "p1", ptr("m1"), nil, "gpt-4", "")

	result := AggregateAllowedModelConfigs([]tables.TableAllowedModelConfig{header, modelRow})
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	if got.Weight == nil || *got.Weight != weight {
		t.Errorf("expected Weight=%v, got %v", weight, got.Weight)
	}
	if got.AllowAllKeys != false {
		t.Errorf("expected AllowAllKeys=false from header row")
	}
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "gpt-4" {
		t.Errorf("expected AllowedModels=[\"gpt-4\"], got %v", got.AllowedModels)
	}
}

// TestAggregateAllowedModelConfigs_MultipleProviders — rows for two providers produce two outputs.
func TestAggregateAllowedModelConfigs_MultipleProviders(t *testing.T) {
	rows := []tables.TableAllowedModelConfig{
		makeRow("vk1", "p1", ptr("m1"), nil, "gpt-4", ""),
		makeRow("vk1", "p2", ptr("m2"), nil, "claude-3", ""),
	}
	result := AggregateAllowedModelConfigs(rows)
	if len(result) != 2 {
		t.Errorf("expected 2 results (one per provider), got %d", len(result))
	}
}

// TestAggregateAllowedModelConfigs_StarAllowedModel — a model named "*" passes through.
// Since AllowedModelID is set, hasAllowRef is true. The governance resolver already
// treats AllowedModels=["*"] as "allow all models", so this is valid end-state.
func TestAggregateAllowedModelConfigs_StarAllowedModel(t *testing.T) {
	row := tables.TableAllowedModelConfig{
		ID:             "r1",
		VirtualKeyID:   ptr("vk1"),
		ProviderID:     "p1",
		AllowedModelID: ptr("m-star"),
		AllowedModel:   &tables.TableModel{ID: "m-star", Name: "*"},
	}
	result := AggregateAllowedModelConfigs([]tables.TableAllowedModelConfig{row})
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	got := result[0]
	// The "*" name is propagated; governance resolver interprets it as allow-all.
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "*" {
		t.Errorf("expected AllowedModels=[\"*\"], got %v", got.AllowedModels)
	}
}

// TestAggregateAllowedModelConfigs_OrgScope — scope_org_id rows are grouped correctly.
func TestAggregateAllowedModelConfigs_OrgScope(t *testing.T) {
	makeOrgRow := func(orgID, providerID string, allowedModelID *string, allowedName string) tables.TableAllowedModelConfig {
		row := tables.TableAllowedModelConfig{
			ID:             "org-row-" + orgID + "-" + providerID,
			ScopeOrgID:     ptr(orgID),
			ProviderID:     providerID,
			AllowedModelID: allowedModelID,
		}
		if allowedModelID != nil && allowedName != "" {
			row.AllowedModel = &tables.TableModel{ID: *allowedModelID, Name: allowedName}
		}
		return row
	}

	rows := []tables.TableAllowedModelConfig{
		makeOrgRow("org1", "p1", ptr("m1"), "gpt-4"),
		makeOrgRow("org1", "p1", ptr("m2"), "gpt-4o"),
		makeOrgRow("org2", "p1", ptr("m3"), "gpt-3"),
	}
	result := AggregateAllowedModelConfigs(rows)
	if len(result) != 2 {
		t.Errorf("expected 2 results (org1/p1 and org2/p1), got %d: %+v", len(result), result)
	}

	byOrg := make(map[string]schemas.WhiteList)
	for _, r := range result {
		if r.ScopeOrgID != nil {
			byOrg[*r.ScopeOrgID] = r.AllowedModels
		}
	}
	if len(byOrg["org1"]) != 2 {
		t.Errorf("expected 2 allowed models for org1, got %v", byOrg["org1"])
	}
	if len(byOrg["org2"]) != 1 {
		t.Errorf("expected 1 allowed model for org2, got %v", byOrg["org2"])
	}
}
