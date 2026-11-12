package guardrails

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// StageResult is the combined outcome of every policy that ran for one stage.
type StageResult struct {
	Decision    string
	PolicyNames []string
	Details     []verdictDetail
	Messages    []MessageText
	OutputText  string
}

type verdictDetail struct {
	PolicyID   string   `json:"policy_id"`
	PolicyName string   `json:"policy_name"`
	Decision   string   `json:"decision"`
	Reasons    []string `json:"reasons,omitempty"`
	LatencyMs  int64    `json:"latency_ms"`
	Error      string   `json:"error,omitempty"`
}

func (p *Plugin) evaluateStage(ctx context.Context, policies []Policy, messages []MessageText, stage string) StageResult {
	result := StageResult{Decision: DecisionAllow, Messages: append([]MessageText(nil), messages...)}
	if len(policies) == 0 {
		return result
	}
	groups := map[int][]Policy{}
	var priorities []int
	for _, policy := range policies {
		if _, ok := groups[policy.Priority]; !ok {
			priorities = append(priorities, policy.Priority)
		}
		groups[policy.Priority] = append(groups[policy.Priority], policy)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	current := result.Messages
	for _, priority := range priorities {
		outcomes := p.evaluateGroup(ctx, groups[priority], current, stage)
		for _, outcome := range outcomes {
			result.Details = append(result.Details, outcome.detail)
			p.metrics.record(outcome.policy.Type, stage, outcome.detail.Decision, outcome.detail.LatencyMs)
			if outcome.detail.Decision == DecisionAllow {
				continue
			}
			result.PolicyNames = append(result.PolicyNames, outcome.policy.Name)
			if outcome.detail.Decision == "fail_open" && result.Decision == DecisionAllow {
				result.Decision = "fail_open"
			}
			if outcome.detail.Decision == DecisionRedact && len(outcome.messages) > 0 {
				current = outcome.messages
			}
			if rank(outcome.detail.Decision) > rank(result.Decision) {
				result.Decision = outcome.detail.Decision
			}
		}
		if result.Decision == DecisionBlock {
			break
		}
	}
	result.Messages = current
	if len(current) > 0 {
		result.OutputText = current[len(current)-1].Text
	}
	return result
}

type groupOutcome struct {
	policy   Policy
	detail   verdictDetail
	messages []MessageText
}

func (p *Plugin) evaluateGroup(ctx context.Context, policies []Policy, messages []MessageText, stage string) []groupOutcome {
	outcomes := make([]groupOutcome, len(policies))
	var wg sync.WaitGroup
	for i, policy := range policies {
		wg.Add(1)
		go func(i int, policy Policy) {
			defer wg.Done()
			outcomes[i] = p.evaluateOne(ctx, policy, messages, stage)
		}(i, policy)
	}
	wg.Wait()
	return outcomes
}

func (p *Plugin) evaluateOne(ctx context.Context, policy Policy, messages []MessageText, stage string) groupOutcome {
	detail := verdictDetail{PolicyID: policy.ID, PolicyName: policy.Name, Decision: DecisionAllow}
	timeout := time.Duration(policy.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	evalCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	evaluator, ok := p.evaluators.Get(policy.Type)
	start := time.Now()
	if !ok {
		detail.Error = "unknown guardrail type"
		detail.Decision = failDecision(policy)
		detail.LatencyMs = time.Since(start).Milliseconds()
		return groupOutcome{policy: policy, detail: detail}
	}
	input := EvaluationInput{Stage: stage, Messages: trimMessages(messages, policy.MaxTurns)}
	evaluation, err := evaluator.Evaluate(evalCtx, policy, input)
	detail.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		detail.Error = err.Error()
		detail.Decision = failDecision(policy)
		return groupOutcome{policy: policy, detail: detail}
	}
	if !evaluation.Matched {
		return groupOutcome{policy: policy, detail: detail}
	}
	detail.Reasons = evaluation.Reasons
	detail.Decision = policy.Action
	if policy.Action != DecisionRedact {
		return groupOutcome{policy: policy, detail: detail}
	}
	copied := append([]MessageText(nil), messages...)
	if replacement := strings.TrimSpace(evaluation.Replacement); replacement != "" && len(copied) > 0 {
		copied[len(copied)-1].Text = replacement
		return groupOutcome{policy: policy, detail: detail, messages: copied}
	}
	if policy.Type == "regex_keyword" {
		redacted, err := redactMessages(copied, policy.ConfigJSON)
		if err == nil {
			return groupOutcome{policy: policy, detail: detail, messages: redacted}
		}
	}
	for i := range copied {
		copied[i].Text = "[REDACTED]"
	}
	return groupOutcome{policy: policy, detail: detail, messages: copied}
}

func failDecision(policy Policy) string {
	if policy.FailClosed {
		return DecisionBlock
	}
	return "fail_open"
}

func rank(decision string) int {
	switch decision {
	case DecisionBlock:
		return 3
	case DecisionRedact:
		return 2
	case DecisionFlag:
		return 1
	default:
		return 0
	}
}
