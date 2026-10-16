package upstream

import "encoding/json"

// normalizeUsageCacheAliases keeps cache-hit aliases consistent before the
// response leaves the gateway. Some WorkBuddy responses carry the real hit in
// prompt_tokens_details.cached_tokens while also emitting
// cache_read_input_tokens: 0 and cached_tokens: 0 compatibility aliases.
// Strict downstream parsers may prefer those zero aliases and lose the hit.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// Responses API consumers use this nested form. Preserve it when the
	// upstream already supplies it, but do not invent it for Chat-only clients.
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}

// UsagePromptTokens 返回 usage 顶层的 prompt_tokens；缺失或非数值时 ok=false。
func UsagePromptTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	return usageNumber(usage, "prompt_tokens")
}

// UsageCacheHitTokens 返回 usage 里的缓存命中 token 数（多别名取最优，口径与
// 回写给客户端的 normalizeUsageCacheAliases 一致）。供网关统计层（usage 桶 /
// reqlog）观测命中率使用；usage 缺失该维度时 ok=false。
func UsageCacheHitTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	return bestUsageCacheHitTokens(usage)
}

// UsageCacheMissTokens 返回 usage 里的缓存未命中 token 数：优先读上游显式的
// prompt_cache_miss_tokens，缺失时按 prompt_tokens - 命中 推导（推导值为负时
// 视为不可信，返回 ok=false）。
func UsageCacheMissTokens(usage map[string]any) (float64, bool) {
	if usage == nil {
		return 0, false
	}
	if miss, ok := usageNumber(usage, "prompt_cache_miss_tokens"); ok {
		return miss, true
	}
	prompt, okP := usageNumber(usage, "prompt_tokens")
	hit, okH := UsageCacheHitTokens(usage)
	if okP && okH && prompt-hit >= 0 {
		return prompt - hit, true
	}
	return 0, false
}

// usageNumber 从 usage 顶层取数值字段（JSON 数字可能是 float64 / json.Number 形态）。
func usageNumber(usage map[string]any, key string) (float64, bool) {
	v, ok := usage[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
