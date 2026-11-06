package guardrails

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

type regexConfig struct {
	Patterns []struct {
		Pattern     string `json:"pattern"`
		Description string `json:"description"`
	} `json:"patterns"`
	Keywords    []string `json:"keywords"`
	Replacement string   `json:"replacement"`
}

type compiledPattern struct {
	re          *regexp.Regexp
	description string
}

type regexEvaluator struct{}

func (regexEvaluator) Type() string { return "regex_keyword" }

func (regexEvaluator) Info() TypeInfo {
	return typeInfo("regex_keyword", "Match regular expressions or keywords in the message text.", bothStages(), []SettingField{
		{Name: "patterns", Type: "pattern_list", Description: "Regular expressions"},
		{Name: "keywords", Type: "string_list", Description: "Case-insensitive keywords"},
		{Name: "replacement", Type: "string", Description: "Text written over a match when the action is redact", Default: "[REDACTED]"},
	})
}

func (regexEvaluator) Evaluate(_ context.Context, policy Policy, input EvaluationInput) (Evaluation, error) {
	patterns, keywords, _, err := compileRegexPolicy(policy.ConfigJSON)
	if err != nil {
		return Evaluation{}, err
	}
	var reasons []string
	for _, message := range input.Messages {
		reasons = append(reasons, matchReasons(message.Text, patterns, keywords)...)
	}
	return Evaluation{Matched: len(reasons) > 0, Reasons: reasons}, nil
}

func compileRegexPolicy(configJSON string) ([]compiledPattern, []string, string, error) {
	var config regexConfig
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			return nil, nil, "", err
		}
	}
	patterns := make([]compiledPattern, 0, len(config.Patterns))
	for _, pattern := range config.Patterns {
		if strings.TrimSpace(pattern.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(pattern.Pattern)
		if err != nil {
			return nil, nil, "", err
		}
		description := pattern.Description
		if description == "" {
			description = pattern.Pattern
		}
		patterns = append(patterns, compiledPattern{re: re, description: description})
	}
	replacement := config.Replacement
	if replacement == "" {
		replacement = "[REDACTED]"
	}
	return patterns, config.Keywords, replacement, nil
}

func matchReasons(text string, patterns []compiledPattern, keywords []string) []string {
	if text == "" {
		return nil
	}
	var reasons []string
	for _, pattern := range patterns {
		if pattern.re.MatchString(text) {
			reasons = append(reasons, pattern.description)
		}
	}
	lower := strings.ToLower(text)
	for _, keyword := range keywords {
		trimmed := strings.TrimSpace(keyword)
		if trimmed != "" && strings.Contains(lower, strings.ToLower(trimmed)) {
			reasons = append(reasons, trimmed)
		}
	}
	return reasons
}

func redactMessages(messages []MessageText, configJSON string) ([]MessageText, error) {
	patterns, keywords, replacement, err := compileRegexPolicy(configJSON)
	if err != nil {
		return nil, err
	}
	out := append([]MessageText(nil), messages...)
	for i := range out {
		updated := out[i].Text
		for _, pattern := range patterns {
			updated = pattern.re.ReplaceAllString(updated, replacement)
		}
		lower := strings.ToLower(updated)
		for _, keyword := range keywords {
			trimmed := strings.TrimSpace(keyword)
			if trimmed == "" {
				continue
			}
			idx := strings.Index(lower, strings.ToLower(trimmed))
			if idx < 0 {
				continue
			}
			updated = updated[:idx] + replacement + updated[idx+len(trimmed):]
			lower = strings.ToLower(updated)
		}
		out[i].Text = updated
	}
	return out, nil
}
