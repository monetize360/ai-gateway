package tables

import "testing"

func TestSyncProviderTypeAssociations_StandardProviderDerivesName(t *testing.T) {
	openaiID := "d2803e2c-5ae8-4496-a264-c979c5be5d30"
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
	if p.ProviderType == nil || *p.ProviderType != "d3fa620b-10f4-4135-93c6-52f22099a5a9" {
		t.Fatalf("ProviderType = %v, want anthropic picklist id", p.ProviderType)
	}
	if p.Name != "anthropic" {
		t.Fatalf("Name = %q, want anthropic", p.Name)
	}
}

func TestRuntimeProviderKey_FromProviderType(t *testing.T) {
	openaiID := "d2803e2c-5ae8-4496-a264-c979c5be5d30"
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
