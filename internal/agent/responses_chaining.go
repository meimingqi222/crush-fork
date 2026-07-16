package agent

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
)

// responsesChainingInput carries the per-step state applyResponsesChaining
// needs to decide whether to send an incremental turn (previous_response_id)
// instead of the full replayed transcript.
type responsesChainingInput struct {
	sessionID string
	// baseOptions is the run-level provider options (with store already
	// forced on when chaining is enabled). The chained options are cloned
	// from these so all other provider settings are preserved.
	baseOptions fantasy.ProviderOptions
	stepNumber  int
	steps       []fantasy.StepResult
	// contextInjected is true when this step appended genuinely new context
	// (memory recall, MCP change notice, queued user prompts, budget steer).
	// Such context is not present in the server-side chained state, so the
	// step must fall back to a full replay to avoid dropping it.
	contextInjected bool
}

// responsesChainingOptions returns the OpenAI Responses provider options
// carried by providerOptions, if any. Its presence is the signal that the
// active model speaks the Responses API and can chain via previous_response_id.
func responsesChainingOptions(providerOptions fantasy.ProviderOptions) (*openai.ResponsesProviderOptions, bool) {
	if providerOptions == nil {
		return nil, false
	}
	opts, ok := providerOptions[openai.Name]
	if !ok {
		return nil, false
	}
	typed, ok := opts.(*openai.ResponsesProviderOptions)
	return typed, ok
}

// enableResponsesStore returns a copy of providerOptions with the Responses
// API `store` flag forced on, so every response in the run is persisted
// server-side and stays chainable via previous_response_id — including steps
// that fall back to a full replay. Returns the input unchanged when there are
// no Responses options.
func enableResponsesStore(providerOptions fantasy.ProviderOptions) fantasy.ProviderOptions {
	base, ok := responsesChainingOptions(providerOptions)
	if !ok {
		return providerOptions
	}
	cloned := *base
	storeOn := true
	cloned.Store = &storeOn
	out := make(fantasy.ProviderOptions, len(providerOptions))
	for k, v := range providerOptions {
		out[k] = v
	}
	out[openai.Name] = &cloned
	return out
}

// responseIDFromMetadata extracts the OpenAI Responses response id from step
// provider metadata, or "" when absent.
func responseIDFromMetadata(metadata fantasy.ProviderMetadata) string {
	if metadata == nil {
		return ""
	}
	data, ok := metadata[openai.Name]
	if !ok {
		return ""
	}
	meta, ok := data.(*openai.ResponsesProviderMetadata)
	if !ok {
		return ""
	}
	return meta.ResponseID
}

// toolCallIDSet collects the tool-call ids emitted by a completed step.
func toolCallIDSet(step fantasy.StepResult) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, tc := range step.Content.ToolCalls() {
		if tc.ToolCallID != "" {
			ids[tc.ToolCallID] = struct{}{}
		}
	}
	return ids
}

// toolResultMessagesForIDs returns the Tool-role messages in msgs that carry a
// result for one of ids, along with the number of matched results. Used to
// build the incremental turn (function_call_output items) for within-run
// chaining.
func toolResultMessagesForIDs(msgs []fantasy.Message, ids map[string]struct{}) ([]fantasy.Message, int) {
	var out []fantasy.Message
	matched := 0
	for _, m := range msgs {
		if m.Role != fantasy.MessageRoleTool {
			continue
		}
		keep := false
		for _, part := range m.Content {
			tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			if _, want := ids[tr.ToolCallID]; want {
				keep = true
				matched++
			}
		}
		if keep {
			out = append(out, m)
		}
	}
	return out, matched
}

// newUserTurnTail returns the User-role messages that follow the last
// Assistant message in msgs — the new turn since the previous stored response.
// It returns nil (bail to full replay) when there is no prior assistant turn
// or when anything other than System (prompt prefix/suffix, already
// server-side) or User messages trails the last assistant.
func newUserTurnTail(msgs []fantasy.Message) []fantasy.Message {
	lastAssistant := -1
	for i, m := range msgs {
		if m.Role == fantasy.MessageRoleAssistant {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return nil
	}
	var tail []fantasy.Message
	for _, m := range msgs[lastAssistant+1:] {
		switch m.Role {
		case fantasy.MessageRoleUser:
			tail = append(tail, m)
		case fantasy.MessageRoleSystem:
			// Prompt prefix/suffix already live server-side under the chained
			// response; do not resend them in the incremental turn.
			continue
		default:
			// A tool result (or anything unexpected) after the last assistant
			// means this is not a clean new-user turn.
			return nil
		}
	}
	return tail
}

// applyResponsesChaining rewrites prepared to send only the incremental turn
// via previous_response_id, when it is safe to do so. It returns true when
// chaining was applied. On any doubt it returns false, leaving prepared for a
// full replay (which the base options still store, keeping the chain alive).
func (a *sessionAgent) applyResponsesChaining(prepared *fantasy.PrepareStepResult, in responsesChainingInput) bool {
	base, ok := responsesChainingOptions(in.baseOptions)
	if !ok {
		return false
	}
	prevID, ok := a.lastResponseID.Get(in.sessionID)
	if !ok || prevID == "" {
		// No stored response to chain from yet. The base options still store
		// this response, so a later step/turn can chain from it.
		return false
	}
	if in.contextInjected {
		return false
	}

	var delta []fantasy.Message
	if in.stepNumber > 0 {
		// Within-run: reply to the previous step's tool calls with only their
		// outputs (function_call_output items).
		if len(in.steps) == 0 {
			return false
		}
		ids := toolCallIDSet(in.steps[len(in.steps)-1])
		if len(ids) == 0 {
			return false
		}
		results, matched := toolResultMessagesForIDs(prepared.Messages, ids)
		if len(results) == 0 || matched < len(ids) {
			// Missing an output for one of the calls; replay in full.
			return false
		}
		delta = results
	} else {
		// Cross-turn: send only the new user turn since the last stored
		// response from the previous turn.
		delta = newUserTurnTail(prepared.Messages)
		if len(delta) == 0 {
			return false
		}
	}

	cloned := *base
	chainedID := prevID
	cloned.PreviousResponseID = &chainedID
	storeOn := true
	cloned.Store = &storeOn

	out := make(fantasy.ProviderOptions, len(in.baseOptions))
	for k, v := range in.baseOptions {
		out[k] = v
	}
	out[openai.Name] = &cloned

	prepared.ProviderOptions = out
	prepared.Messages = delta
	return true
}
