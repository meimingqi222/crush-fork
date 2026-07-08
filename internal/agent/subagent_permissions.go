package agent

import (
	"slices"
	"strings"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
)

type ParentPermissionContext struct {
	SessionID    string
	AgentName    string
	AllowedTools []string
	DeniedTools  []string
	ExternalDeny []string
	Mode         string
}

func DeriveSubagentPermissions(parent ParentPermissionContext, profile SubagentProfile, availableTools []string) DerivedSubagentPermissions {
	allowed := intersectToolNames(profile.ToolNames, availableTools)
	if len(allowed) == 0 {
		allowed = normalizeToolNames(availableTools)
	}
	if parentAllowed := normalizeToolNames(parent.AllowedTools); len(parentAllowed) > 0 {
		allowed = intersectToolNames(allowed, parentAllowed)
	}
	allowed = unionToolNames(allowed, mandatorySubagentToolNames())

	// Dynamically preserve non-builtin tools (e.g. MCP and custom plugin tools) that are available.
	// These tools are not listed in the static profile.ToolNames by default, but should be allowed
	// for the subagent if they are available in parent.AllowedTools.
	var nonBuiltinTools []string
	parentAllowedSet := toToolSet(parent.AllowedTools)
	for _, toolName := range availableTools {
		if !config.IsBuiltinTool(toolName) {
			if len(parentAllowedSet) > 0 {
				if _, ok := parentAllowedSet[toolName]; !ok {
					continue
				}
			}
			nonBuiltinTools = append(nonBuiltinTools, toolName)
		}
	}
	if len(nonBuiltinTools) > 0 {
		allowed = unionToolNames(allowed, nonBuiltinTools)
	}

	externalDeny := normalizeToolNames(parent.ExternalDeny)
	_, agentExternallyDenied := toToolSet(externalDeny)[AgentToolName]
	denied := unionToolNames(
		parent.DeniedTools,
		externalDeny,
		profile.DenyTools,
		globalSubagentDeniedTools(),
	)
	if profile.ReadOnly {
		denied = unionToolNames(denied, readOnlyDeniedToolNames())
	}
	if !profile.CanSpawn {
		denied = unionToolNames(denied, []string{AgentToolName})
	}

	allowed = subtractToolNames(allowed, denied)
	return DerivedSubagentPermissions{
		AllowedTools:              toToolSet(allowed),
		DeniedTools:               toToolSet(denied),
		ReadOnly:                  profile.ReadOnly,
		CanSpawn:                  profile.CanSpawn,
		AgentToolExternallyDenied: agentExternallyDenied,
	}
}

func subagentToolProfileFromPermissions(permissions DerivedSubagentPermissions) SubagentToolProfile {
	return SubagentToolProfile{
		Allowed: cloneToolSet(permissions.AllowedTools),
		Denied:  cloneToolSet(permissions.DeniedTools),
	}
}

func mandatorySubagentToolNames() []string {
	return []string{agenttools.YieldToolName}
}

func globalSubagentDeniedTools() []string {
	return []string{
		agenttools.ResolveToolName,
		agenttools.RequestUserInputToolName,
		// goal is denied for all subagents because subagent runs invoke
		// params.Agent.Run directly, bypassing coordinator.Run and the
		// goalRuntime OnTurnStart/PostTurn accounting. A goal created by a
		// subagent would silently never accumulate tokens/time and never
		// trigger continuation — dead surface area that confuses callers.
		// Denial is the safe fix until subagent-owned goals have a real
		// use case that warrants routing through the goal runtime.
		agenttools.GoalToolName,
	}
}

func readOnlyDeniedToolNames() []string {
	return []string{
		agenttools.DownloadToolName,
		agenttools.EditToolName,
		agenttools.WriteToolName,
		agenttools.RetainToolName,
		agenttools.TodosToolName,
		agenttools.SendMessageToolName,
		agenttools.TaskStopToolName,
		agenttools.LSPToolName,
		agenttools.JobToolName,
		agenttools.IrcToolName,
	}
}

func normalizeToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	slices.Sort(normalized)
	return normalized
}

func toolNamesFromSet(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	return normalizeToolNames(names)
}

func intersectToolNames(left, right []string) []string {
	left = normalizeToolNames(left)
	right = normalizeToolNames(right)
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	rightSet := toToolSet(right)
	intersected := make([]string, 0, len(left))
	for _, name := range left {
		if _, ok := rightSet[name]; ok {
			intersected = append(intersected, name)
		}
	}
	return intersected
}

func unionToolNames(groups ...[]string) []string {
	seen := make(map[string]struct{})
	union := make([]string, 0)
	for _, group := range groups {
		for _, name := range normalizeToolNames(group) {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			union = append(union, name)
		}
	}
	slices.Sort(union)
	return union
}

func subtractToolNames(values, denied []string) []string {
	values = normalizeToolNames(values)
	if len(values) == 0 {
		return nil
	}
	if len(denied) == 0 {
		return values
	}
	deniedSet := toToolSet(denied)
	filtered := make([]string, 0, len(values))
	for _, name := range values {
		if _, ok := deniedSet[name]; ok {
			continue
		}
		filtered = append(filtered, name)
	}
	return filtered
}

func toToolSet(names []string) map[string]struct{} {
	names = normalizeToolNames(names)
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}
