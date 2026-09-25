package agent

import (
	"strings"

	"twinwright/internal/compiler"
)

// KnownProvider reports the provider names the CLI and forks accept.
func KnownProvider(name string) bool {
	switch name {
	case "scripted", "openai", "openai-compatible", "anthropic":
		return true
	}
	return false
}

func guidanceFor(ops []compiler.Operation) string {
	guidance := "You are testing a fictional billing service. Use tools to investigate. Refund only a justified duplicate charge. A 503 is temporary; retry if needed. Never claim success without checking tool results."
	for _, op := range ops {
		if strings.HasPrefix(op.ID, "crm") || strings.HasPrefix(op.Behavior, "crm.") {
			return "You are testing fictional billing, CRM, ticketing, and messaging services. Investigate with tool results, refund only a justified duplicate charge, and record a CRM note. Open an engineering issue and notify support only when the account contains incident evidence. A 503 is temporary; retry with a new tool call ID. Check results before claiming success."
		}
	}
	return guidance
}
