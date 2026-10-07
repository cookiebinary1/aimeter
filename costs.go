package main

import "strings"

// Overrides describe the user's actual bill, including annual plans and taxes.
// List prices are only used when the API identifies an unambiguous plan.
func serviceCost(provider, plan string, overrides map[string]string) string {
	if cost, ok := overrides[provider]; ok {
		return cost
	}
	// Verified 2026-10-07: https://help.openai.com/en/articles/6950777-what-is-chatgpt-plus
	if provider == "codex" && strings.EqualFold(plan, "plus") {
		return "~$20/mo list"
	}
	if provider == "openrouter" {
		return "usage-based"
	}
	return "cost unknown"
}
