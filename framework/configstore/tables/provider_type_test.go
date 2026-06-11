package tables

import "testing"

func TestSyncProviderTypeAssociations_StandardProviderDerivesName(t *testing.T) {
	openaiID := "145e3a40-2018-4f73-a51e-ffb63a79e861"
	p := &TableProvider{
		ProviderType: &openaiID,
		Name:         "wrong-name",
	}
	if err := p.SyncProviderTypeAssociations(); err != nil {
		t.Fatalf("SyncProviderTypeAssociations() error = %v", err)
	}
	if p.Name != "openai" {
		t.Fatalf("Name = %q, want openai", p.Name)
	}
}

func TestSyncProviderTypeAssociations_CustomProviderKeepsName(t *testing.T) {
	customID := CustomProviderPicklistItemID
	p := &TableProvider{
		ProviderType: &customID,
		Name:         "my-custom-gateway",
		CustomProviderConfigJSON: `{"base_provider_type":"openai","is_key_less":true}`,
	}
	if err := p.SyncProviderTypeAssociations(); err != nil {
		t.Fatalf("SyncProviderTypeAssociations() error = %v", err)
	}
	if p.Name != "my-custom-gateway" {
		t.Fatalf("Name = %q, want my-custom-gateway", p.Name)
	}
}

func TestSyncProviderTypeAssociations_BackfillTypeFromStandardName(t *testing.T) {
	p := &TableProvider{Name: "anthropic"}
	if err := p.SyncProviderTypeAssociations(); err != nil {
		t.Fatalf("SyncProviderTypeAssociations() error = %v", err)
	}
	if p.ProviderType == nil || *p.ProviderType != "94e96a44-740d-4372-a0e0-bc9837173c3a" {
		t.Fatalf("ProviderType = %v, want anthropic picklist id", p.ProviderType)
	}
	if p.Name != "anthropic" {
		t.Fatalf("Name = %q, want anthropic", p.Name)
	}
}

func TestRuntimeProviderKey_FromProviderType(t *testing.T) {
	openaiID := "145e3a40-2018-4f73-a51e-ffb63a79e861"
	p := &TableProvider{
		ProviderType: &openaiID,
		Name:         "ignored",
	}
	key, err := p.RuntimeProviderKey()
	if err != nil {
		t.Fatalf("RuntimeProviderKey() error = %v", err)
	}
	if key != "openai" {
		t.Fatalf("RuntimeProviderKey() = %q, want openai", key)
	}
}

func TestSyncProviderTypeAssociations_CustomNameCannotMatchStandard(t *testing.T) {
	customID := CustomProviderPicklistItemID
	p := &TableProvider{
		ProviderType: &customID,
		Name:         "openai",
	}
	if err := p.SyncProviderTypeAssociations(); err == nil {
		t.Fatal("expected error when custom provider name matches standard provider")
	}
}
