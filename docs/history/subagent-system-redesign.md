# Subagent System Redesign

> **HISTORICAL - DO NOT USE AS REFERENCE.** This is a redesign proposal for
> agent-config extensions (Spawns, Blocking, OutputSchema, ThinkingLevel,
> ModelPriority); only Phase 1-2 were implemented. The authoritative
> current-state reference is `docs/subagent-runtime.md`.

## Problem Statement

1. **Explore subagent misuse**: The explore subagent runs on a small/fast model but is sometimes used for final code review, which should use a stronger model.

2. **Missing spawns mechanism**: oh-my-pi allows plan/review to spawn explore for parallel evidence gathering. Current implementation lacks this.

3. **No blocking control**: oh-my-pi's reviewer has `blocking: true` to ensure synchronous execution before proceeding.

4. **No output schema**: Each subagent in oh-my-pi defines structured output for consistent results.

5. **Model configuration inflexibility**: Current implementation uses model slots (large/small) instead of direct model names with fallback.

## Proposed Design

### 1. Agent Configuration Extensions

Add new fields to `Agent` struct:

```go
type Agent struct {
    // Existing fields...

    // New fields
    Spawns        []string `json:"spawns,omitempty"`         // Subagent types this agent can spawn ("*" for any)
    Blocking      *bool    `json:"blocking,omitempty"`       // Whether execution blocks main flow
    OutputSchema  string   `json:"output_schema,omitempty"`  // JSON schema for structured output
    ThinkingLevel string   `json:"thinking_level,omitempty"` // "minimal", "low", "medium", "high"
    ModelPriority []string `json:"model_priority,omitempty"` // Model fallback list
}
```

### 2. Subagent Profiles

| Profile    | Model Priority | Thinking | Spawns     | Blocking | Tools                                    |
|------------|---------------|----------|------------|----------|------------------------------------------|
| explore    | small         | medium   | -          | false    | read, glob, grep, bash(git-only)         |
| plan       | plan→large    | high     | explore    | false    | read, glob, grep, bash(git), lsp         |
| review     | review→large  | high     | explore    | true     | read, glob, grep, bash(git), lsp         |
| general    | large         | medium   | -          | false    | all                                      |
| quick_task | small         | minimal  | -          | false    | read, write, edit, bash                  |
| librarian  | small         | medium   | -          | false    | read, glob, grep, web_fetch, agentic_fetch|
| designer   | designer→large| medium   | -          | false    | all                                      |

### 3. Spawns Mechanism

When an agent has `spawns` defined:
1. Auto-include `agent` tool if not at max recursion depth
2. Validate spawn requests against allowed types
3. At max depth, remove agent tool from available tools

```go
func (c *coordinator) resolveAgentTools(agentCfg config.Agent, depth int, maxDepth int) []string {
    tools := agentCfg.AllowedTools

    // Auto-include agent tool if spawns defined and not at max depth
    if len(agentCfg.Spawns) > 0 && depth < maxDepth {
        if !slices.Contains(tools, AgentToolName) {
            tools = append(tools, AgentToolName)
        }
    }

    return tools
}
```

### 4. Blocking Execution

When `blocking: true`:
1. Subagent runs synchronously
2. Main agent waits for completion before proceeding
3. Result must be acknowledged before continuing

### 5. Output Schema

Define structured output schemas per subagent type:

**explore output**:
```json
{
  "summary": "Brief findings summary",
  "files": [{"ref": "path:line", "description": "section contents"}],
  "architecture": "How pieces connect"
}
```

**review output**:
```json
{
  "overall_correctness": "correct|incorrect",
  "explanation": "Verdict summary",
  "confidence": 0.0-1.0,
  "findings": [{"title", "body", "priority", "confidence", "file_path", "line_start", "line_end"}]
}
```

**plan output**:
```json
{
  "summary": "What to build and why",
  "changes": ["file:line ranges"],
  "sequence": ["ordered steps"],
  "edge_cases": ["edge cases to handle"],
  "verification": ["verification steps"],
  "critical_files": ["key files"]
}
```

### 6. Model Resolution

Support model priority lists:

```go
func (c *coordinator) resolveModelForAgent(agentCfg config.Agent) (SelectedModel, error) {
    // If model_priority specified, try each in order
    for _, modelRef := range agentCfg.ModelPriority {
        if model, ok := c.cfg.Config().Models[modelRef]; ok {
            return model, nil
        }
    }

    // Fallback to slot-based resolution
    return c.cfg.Config().SelectedModelForType(agentCfg.Model)
}
```

### 7. Enforcement Rules

Update `validateSubagentDelegations` to:
1. Detect final review patterns and require `review` subagent
2. Block `explore` from tasks requiring strong reasoning
3. Validate spawn requests against allowed types

## Implementation Plan

### Phase 1: Core Infrastructure (config changes)
- [x] Add new fields to Agent struct (Spawns, Blocking, OutputSchema, ThinkingLevel, ModelPriority)
- [x] Update builtinAgents with spawns/blocking/ThinkingLevel/OutputSchema/ModelPriority
- [x] Add model_priority support (ResolveModelForAgent with fallback chain)
- [x] Add output schema templates (buildOutputSchemaPrompt, outputSchemaDefinition)
- [x] Add helper methods (CanSpawn, IsBlocking, ptrBool)
- [x] Update mergeAgentConfig and agentConfigsEqual for new fields

### Phase 2: Spawns Mechanism
- [x] Update SubagentProfile struct (Spawns, Blocking, ThinkingLevel, ResultSchema)
- [x] Update subagentProfileForAgent to populate new fields from Agent config
- [x] Update CanSpawn logic in subagentRuntimePostConfig (Spawns-based + legacy fallback)
- [x] Add buildSpawnsPrompt for system prompt injection
- [x] Update agent_tool.md with spawns/blocking guidance

### Phase 3: Blocking Execution
- [x] Add buildBlockingPrompt for system prompt injection
- [x] Add IsBlocking() helper method on Agent struct
- [ ] Wire blocking flag into task graph execution (TODO: requires coordinator changes for synchronous blocking)

### Phase 4: Output Schema
- [x] Add output schema definitions (explore_output, review_output, plan_output)
- [x] Add buildOutputSchemaPrompt for system prompt injection
- [ ] Add schema validation for subagent_finish (TODO: requires runtime validation)

### Phase 5: Model Resolution
- [x] Implement ResolveModelForAgent with ModelPriority fallback chain
- [x] Add resolveModelForSubagent in handoff.go
- [ ] Wire resolveModelForSubagent into coordinator subagent launch (TODO: coordinator uses selectedModelWithOverride currently)

### Phase 6: Enforcement
- [x] Update agent_tool.md with new guidance for plan/review/designer/librarian/quick_task
- [ ] Add spawn type validation in coordinator (TODO: needs ParentAgentID on taskGraphTask)
- [ ] Strengthen explore misuse detection

## Files to Modify

1. `internal/config/config.go` - Agent struct extensions
2. `internal/agent/subagent_runtime.go` - Spawns/blocking support
3. `internal/agent/agent_tool.go` - Validation, spawn checking
4. `internal/agent/prompts.go` - Output schema injection
5. `internal/agent/templates/agent_tool.md` - Updated documentation
