package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/stringext"
)

func applyRuntimeConfig(call *SessionAgentCall, runtimeConfig sessionAgentRuntimeConfig) {
	if runtimeConfig.ProviderOptions != nil {
		call.ProviderOptions = runtimeConfig.ProviderOptions
	}
	if runtimeConfig.MaxOutputTokens > 0 {
		call.MaxOutputTokens = runtimeConfig.MaxOutputTokens
	}
	if runtimeConfig.Temperature != nil {
		call.Temperature = runtimeConfig.Temperature
	}
	if runtimeConfig.TopP != nil {
		call.TopP = runtimeConfig.TopP
	}
	if runtimeConfig.TopK != nil {
		call.TopK = runtimeConfig.TopK
	}
	if runtimeConfig.FrequencyPenalty != nil {
		call.FrequencyPenalty = runtimeConfig.FrequencyPenalty
	}
	if runtimeConfig.PresencePenalty != nil {
		call.PresencePenalty = runtimeConfig.PresencePenalty
	}
}

func effectiveRuntimeModel(defaultModel Model, runtimeConfig *sessionAgentRuntimeConfig) Model {
	if runtimeConfig != nil && runtimeConfig.Model != nil {
		return *runtimeConfig.Model
	}
	return defaultModel
}

func (a *sessionAgent) refreshCallConfigIfNeeded(ctx context.Context, call *SessionAgentCall) (*sessionAgentRuntimeConfig, error) {
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(*sessionAgentRuntimeConfig); ok && runtimeConfig != nil {
		applyRuntimeConfig(call, *runtimeConfig)
		return runtimeConfig, nil
	}
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(sessionAgentRuntimeConfig); ok {
		applyRuntimeConfig(call, runtimeConfig)
		return &runtimeConfig, nil
	}
	if a.refreshCallConfig == nil {
		return nil, nil
	}
	runtimeConfig, err := a.refreshCallConfig(ctx)
	if err != nil {
		return nil, err
	}
	applyRuntimeConfig(call, runtimeConfig)
	return &runtimeConfig, nil
}

func (a *sessionAgent) resetRetriedStep(ctx context.Context, assistant *message.Message, toolMessageIDs []string) error {
	for _, toolMessageID := range toolMessageIDs {
		if err := a.messages.Delete(ctx, toolMessageID); err != nil {
			return err
		}
	}
	assistant.Parts = nil
	return a.messages.Update(ctx, *assistant)
}

func (a *sessionAgent) SetModels(large Model, small Model) {
	a.largeModel.Set(large)
	a.smallModel.Set(small)
}

func (a *sessionAgent) SetTools(tools []fantasy.AgentTool) {
	a.tools.SetSlice(tools)
}

func (a *sessionAgent) SetSystemPrompt(systemPrompt string) {
	old := a.systemPrompt.Get()
	a.systemPrompt.Set(systemPrompt)
	// Only invalidate the enhanced system prompt cache if the base
	// prompt actually changed. The coordinator calls SetSystemPrompt on
	// every Run(), so unconditional invalidation would defeat the cache
	// entirely — the enhanced prompt would be rebuilt from scratch each
	// time, causing the instructions hash to change and breaking prompt
	// caching on the provider side.
	if old != systemPrompt {
		oldHash := sha256.Sum256([]byte(old))
		newHash := sha256.Sum256([]byte(systemPrompt))
		slog.Info("[CACHE-DIAG] SetSystemPrompt changed, invalidating enhanced cache",
			"old_len", len(old),
			"new_len", len(systemPrompt),
			"old_hash", hex.EncodeToString(oldHash[:8]),
			"new_hash", hex.EncodeToString(newHash[:8]),
		)
		a.invalidateEnhancedSystemPrompt()
	}
}

func (a *sessionAgent) invalidateEnhancedSystemPrompt() {
	a.enhancedPromptMu.Lock()
	a.enhancedSystemPrompt = ""
	a.enhancedPromptContextSig = ""
	a.enhancedPromptMu.Unlock()
}

// buildEnhancedSystemPrompt appends dynamic-but-stable-per-session parts
// (MCP instructions and the vision note) to the base system prompt and caches
// the result. Dynamic memory is injected into the prepared message tail so
// it does not change the stable prompt prefix.
func (a *sessionAgent) buildEnhancedSystemPrompt(ctx context.Context, basePrompt string, largeModel Model, contextSig string) string {
	a.enhancedPromptMu.Lock()
	defer a.enhancedPromptMu.Unlock()
	if a.enhancedSystemPrompt != "" && a.enhancedPromptContextSig == contextSig {
		return a.enhancedSystemPrompt
	}

	enhanced := basePrompt

	// Collect MCP instructions in deterministic (alphabetical) order.
	// mcp.GetStates() returns a map whose iteration order is random in Go,
	// so without sorting the concatenated instructions would differ between
	// rebuilds, changing the system prompt hash and breaking prompt caching.
	mcpStates := mcp.GetStates()
	mcpNames := make([]string, 0, len(mcpStates))
	for name := range mcpStates {
		if !tools.MCPServerAllowed(ctx, name) {
			continue
		}
		mcpNames = append(mcpNames, name)
	}
	slices.Sort(mcpNames)

	var instructions strings.Builder
	for _, name := range mcpNames {
		server := mcpStates[name]
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			instructions.WriteString(s)
			instructions.WriteString("\n\n")
		}
	}
	if s := instructions.String(); s != "" {
		enhanced += "\n\n<mcp-instructions>\n" + s + "\n</mcp-instructions>"
	}

	if !largeModel.CatwalkCfg.SupportsImages && a.visionService != nil && a.visionService.IsAvailable() {
		enhanced += "\n\n" + describeImageToolSystemPromptNote()
	}

	a.enhancedSystemPrompt = enhanced
	a.enhancedPromptContextSig = contextSig

	// Diagnostic: log hash and section sizes to identify what changes
	// between rebuilds. This helps pinpoint which dynamic section is
	// causing the instructions hash to vary across Run() calls.
	baseLen := len(basePrompt)
	mcpLen := instructions.Len()
	h := sha256.Sum256([]byte(enhanced))
	slog.Info("[CACHE-DIAG] enhancedSystemPrompt rebuilt",
		"hash", hex.EncodeToString(h[:8]),
		"total_len", len(enhanced),
		"base_len", baseLen,
		"mcp_len", mcpLen,
		"mcp_servers_connected", len(mcpNames),
	)

	return enhanced
}

// mcpStateSnapshot captures the MCP server states at a point in time.
// Used to detect changes during a run so we can notify the agent.
type mcpStateSnapshot struct {
	states   map[string]mcp.State
	revision uint64
}

// snapshotMcpStates records the current MCP server states.
func snapshotMcpStates(ctx context.Context) mcpStateSnapshot {
	snapshot := mcpStateSnapshot{
		states:   make(map[string]mcp.State),
		revision: tools.MCPServerAccessRevision(ctx),
	}
	for name, info := range mcp.GetStates() {
		if !tools.MCPServerAllowed(ctx, name) {
			continue
		}
		snapshot.states[name] = info.State
	}
	return snapshot
}

// mcpStateChange describes a single MCP server state transition.
type mcpStateChange struct {
	Name string
	From mcp.State
	To   mcp.State
}

// diffMcpStates compares the current MCP states against a snapshot and
// returns the changes (new servers, disconnected servers, state transitions).
func diffMcpStates(ctx context.Context, snapshot mcpStateSnapshot) ([]mcpStateChange, bool) {
	current := mcp.GetStates()
	var changes []mcpStateChange

	// Check for new servers or state transitions.
	for name, info := range current {
		if !tools.MCPServerAllowed(ctx, name) {
			continue
		}
		prevState, existed := snapshot.states[name]
		if !existed {
			changes = append(changes, mcpStateChange{Name: name, From: mcp.StateDisabled, To: info.State})
		} else if prevState != info.State {
			changes = append(changes, mcpStateChange{Name: name, From: prevState, To: info.State})
		}
	}
	// Check for servers that disappeared (disconnected/removed).
	for name, prevState := range snapshot.states {
		if _, exists := current[name]; !exists {
			changes = append(changes, mcpStateChange{Name: name, From: prevState, To: mcp.StateDisabled})
		}
	}

	return changes, snapshot.revision != tools.MCPServerAccessRevision(ctx)
}

// formatMcpChangeNotification creates a System Message informing the agent
// about MCP server state changes. This is injected as a trailing system
// message to preserve the prompt cache prefix.
func formatMcpChangeNotification(changes []mcpStateChange) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	if len(changes) == 0 {
		b.WriteString("The MCP configuration available to this session has changed. Refresh your tool selection before continuing.\n")
	} else {
		b.WriteString("The following MCP server connections have changed since the conversation started:\n")
	}
	for _, c := range changes {
		if c.To == mcp.StateConnected {
			b.WriteString(fmt.Sprintf("- MCP server \"%s\" is now connected (was: %s). Its tools and instructions are now available.\n", c.Name, c.From))
		} else if c.From == mcp.StateConnected {
			b.WriteString(fmt.Sprintf("- MCP server \"%s\" disconnected (was: connected). Its tools are no longer available.\n", c.Name))
		} else {
			b.WriteString(fmt.Sprintf("- MCP server \"%s\" state changed from %s to %s.\n", c.Name, c.From, c.To))
		}
	}
	b.WriteString("</system-reminder>")
	return b.String()
}

func (a *sessionAgent) SetSystemPromptPrefix(systemPromptPrefix string) {
	a.systemPromptPrefix.Set(systemPromptPrefix)
}

func (a *sessionAgent) Model() Model {
	return a.largeModel.Get()
}

func withRetryFailureDetails(details string, retryAttempt int) string {
	details = strings.TrimSpace(details)
	if retryAttempt <= 0 {
		return details
	}

	summary := fmt.Sprintf("Retried %d %s, but the request still failed.", retryAttempt, cmp.Or(pluralizeRetryAttempt(retryAttempt), "times"))
	if details == "" {
		return summary
	}
	return summary + " " + details
}

func pluralizeRetryAttempt(retryAttempt int) string {
	if retryAttempt == 1 {
		return "time"
	}
	return "times"
}

// providerErrorTitle returns a human-readable title for a provider error.
// It overrides the HTTP status text when the message indicates an upstream
// aggregator failure (no available route / all accounts exhausted) rather
// than a genuine client-side rate limit, so the displayed title matches the
// actual failure reason instead of the misleading "Too Many Requests".
func providerErrorTitle(providerErr *fantasy.ProviderError) string {
	if providerErr != nil && providerErr.Title != "" {
		msg := strings.ToLower(providerErr.Message)
		for _, pattern := range noRoutePatterns {
			if strings.Contains(msg, pattern) {
				return "No available route"
			}
		}
		return stringext.Capitalize(providerErr.Title)
	}
	return ""
}

// noRoutePatterns are message fragments emitted by upstream aggregators
// (e.g. copilot-api, openrouter) when they have no account/route available
// to serve a request. These are typically returned with HTTP 429 even
// though the failure is server-side, not a client rate limit.
var noRoutePatterns = []string{
	"no available route",
	"all accounts exhausted",
	"no available model",
	"no upstream available",
}
