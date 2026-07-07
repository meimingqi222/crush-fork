package planmode

import (
	"strings"

	"github.com/charmbracelet/crush/internal/agent"
)

const (
	ProposedPlanOpenTag  = "<proposed_plan>"
	ProposedPlanCloseTag = "</proposed_plan>"
)

func ExtractProposedPlan(content string) (string, bool) {
	start := strings.Index(content, ProposedPlanOpenTag)
	if start == -1 {
		return "", false
	}
	start += len(ProposedPlanOpenTag)
	end := strings.Index(content[start:], ProposedPlanCloseTag)
	if end == -1 {
		return "", false
	}
	plan := strings.TrimSpace(content[start : start+end])
	if plan == "" {
		return "", false
	}
	return plan, true
}

func WrapProposedPlan(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return ProposedPlanOpenTag + "\n" + ProposedPlanCloseTag
	}
	return ProposedPlanOpenTag + "\n" + plan + "\n" + ProposedPlanCloseTag
}

// ExecutionContextMode controls how the session context is handled when
// transitioning from plan mode to execution.
type ExecutionContextMode int

const (
	// ExecuteDirect switches to default mode and sends the execution prompt
	// without modifying the session context.
	ExecuteDirect ExecutionContextMode = iota
	// ExecuteWithCompact triggers context compaction before execution.
	ExecuteWithCompact
	// ExecuteKeepContext preserves the full exploration history for execution.
	ExecuteKeepContext
)

// BuildExecutionPrompt creates the prompt sent to the agent when the user
// approves a plan and execution begins.
func BuildExecutionPrompt(plan string, mode ExecutionContextMode) string {
	plan = strings.TrimSpace(plan)

	var header string
	switch mode {
	case ExecuteWithCompact:
		header = "Execute the approved plan below. You are no longer in Plan Mode, so you should implement it now. Context has been compacted — the plan file is your source of truth."
	case ExecuteKeepContext:
		header = "Execute the approved plan below. You are no longer in Plan Mode, so you should implement it now. Full exploration context is preserved."
	default:
		header = "Execute the approved plan below. You are no longer in Plan Mode, so you should implement it now."
	}

	if plan == "" {
		return strings.TrimSpace(agent.ApprovedPlanSystemPrompt + "\n\n" + header)
	}
	return strings.TrimSpace(agent.ApprovedPlanSystemPrompt + "\n\n" + header + "\n\nApproved plan:\n" + plan)
}
