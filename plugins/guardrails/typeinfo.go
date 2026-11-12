package guardrails

func bothStages() []string { return []string{"input", "output"} }

func typeInfo(typeName, description string, stages []string, settings []SettingField) TypeInfo {
	return TypeInfo{
		Type:         typeName,
		Description:  description,
		Stages:       stages,
		Settings:     settings,
		ConfigSchema: configSchema(settings),
	}
}

func configSchema(settings []SettingField) map[string]any {
	properties := map[string]any{}
	var required []string
	for _, field := range settings {
		property := map[string]any{"description": field.Description}
		switch field.Type {
		case "number":
			property["type"] = "number"
		case "boolean":
			property["type"] = "boolean"
		case "string_list":
			property["type"] = "array"
			property["items"] = map[string]any{"type": "string"}
		case "pattern_list":
			property["type"] = "array"
			property["items"] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
				},
				"required": []string{"pattern"},
			}
		default:
			property["type"] = "string"
		}
		if len(field.Enum) > 0 {
			property["enum"] = field.Enum
		}
		if field.Default != nil {
			property["default"] = field.Default
		}
		properties[field.Name] = property
		if field.Required {
			required = append(required, field.Name)
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
