package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/crush/internal/message"
)

// subagentYieldProjectionMaxArrayItems caps how many entries of a single
// array field are rendered by projectYieldPayload. Without a cap, a
// pathologically long array (e.g. hundreds of file references) could
// dominate the projected text and blow the per-task output budget on its
// own.
const subagentYieldProjectionMaxArrayItems = 20

// projectYieldPayload deterministically projects a yield's structured
// payload into model-readable text. It never calls a model: this is a pure,
// best-effort rendering used when a subagent only submitted a payload (no
// data), so the parent agent still gets a directly readable result instead
// of a "no textual response" placeholder.
//
// Projection rules (docs/refactor-subagent-result-contract.md §3.3):
//  1. A top-level "summary" string field, when present, opens the text.
//  2. Remaining top-level fields are appended as labeled paragraphs, in
//     schema-declared order when schema is available. Arrays are rendered
//     as bulleted item lists, capped at subagentYieldProjectionMaxArrayItems
//     entries.
//  3. Missing fields are not a failure -- projection is best-effort.
//  4. If nothing renders to readable text, the payload degrades to compact
//     JSON, which is still strictly better than no content at all.
//
// schema is the JSON Schema object the payload was validated against (or
// nil when unavailable). It decides which extra fields to surface and in
// what order. Its "properties" object is a map, so Go does not preserve the
// authored order; its "required" array is a slice and does. Fields are
// therefore rendered in "required" order first -- schema authors put the
// fields that matter most there -- followed by the optional properties and
// then any payload fields the schema does not declare, both alphabetically.
// When schema is nil, the payload's own top-level keys are used, sorted
// alphabetically. Either way the output is stable across repeated calls on
// the same payload -- never dependent on Go map iteration order, which is
// randomized per process.
func projectYieldPayload(payload json.RawMessage, schema map[string]any) string {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return ""
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return ""
	}

	obj, ok := decoded.(map[string]any)
	if !ok {
		// Not a JSON object (array, string, number, ...): there is no
		// schema-shaped structure to project, so fall back to compact JSON.
		return compactJSONFallback(decoded)
	}

	var b strings.Builder
	consumedSummary := false
	if summary, ok := obj["summary"].(string); ok {
		if trimmedSummary := strings.TrimSpace(summary); trimmedSummary != "" {
			b.WriteString(trimmedSummary)
			consumedSummary = true
		}
	}

	for _, key := range projectionFieldOrder(obj, schema) {
		if key == "summary" && consumedSummary {
			continue
		}
		value, ok := obj[key]
		if !ok {
			continue
		}
		section := projectFieldSection(key, value)
		if section == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(section)
	}

	if text := strings.TrimSpace(b.String()); text != "" {
		return text
	}

	// Every field was empty, missing, or unrenderable: still better than
	// nothing to hand the parent model.
	return compactJSONFallback(decoded)
}

// projectionFieldOrder returns the top-level field names to render, in a
// stable order. When schema declares "properties", those names are used
// first (any payload fields not covered by the schema are appended after,
// so unexpected extra data is not silently dropped); otherwise every
// top-level payload key is used. Both branches sort alphabetically because
// Go's map iteration order is randomized and must never leak into
// model-visible text.
func projectionFieldOrder(obj map[string]any, schema map[string]any) []string {
	if schema != nil {
		if props, ok := schema["properties"].(map[string]any); ok && len(props) > 0 {
			// "required" is a JSON array, so unlike "properties" it survives
			// as an ordered slice. Schema authors list the fields that matter
			// most there, so honor that order first and only fall back to
			// alphabetical for the optional remainder.
			declared := make([]string, 0, len(props))
			placed := make(map[string]struct{}, len(props))
			for _, name := range schemaRequiredOrder(schema) {
				if _, isProp := props[name]; !isProp {
					continue
				}
				if _, dup := placed[name]; dup {
					continue
				}
				placed[name] = struct{}{}
				declared = append(declared, name)
			}

			optional := make([]string, 0, len(props))
			for name := range props {
				if _, done := placed[name]; done {
					continue
				}
				optional = append(optional, name)
			}
			sort.Strings(optional)
			declared = append(declared, optional...)

			declaredSet := make(map[string]struct{}, len(declared))
			for _, name := range declared {
				declaredSet[name] = struct{}{}
			}
			var extra []string
			for key := range obj {
				if _, known := declaredSet[key]; !known {
					extra = append(extra, key)
				}
			}
			sort.Strings(extra)
			return append(declared, extra...)
		}
	}

	names := make([]string, 0, len(obj))
	for key := range obj {
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// schemaRequiredOrder returns the schema's "required" field names in their
// declared order. The value is a JSON array, so it may decode as []string
// (Go literal, as in config.go's agent definitions) or []any (parsed from a
// crush.json override); both are accepted. Returns nil when absent.
func schemaRequiredOrder(schema map[string]any) []string {
	switch required := schema["required"].(type) {
	case []string:
		return required
	case []any:
		names := make([]string, 0, len(required))
		for _, entry := range required {
			if name, ok := entry.(string); ok && name != "" {
				names = append(names, name)
			}
		}
		return names
	default:
		return nil
	}
}

// projectFieldSection renders a single labeled field. Returns "" when the
// value carries no readable content (missing, null, empty string, empty
// array, empty object).
func projectFieldSection(key string, value any) string {
	label := projectionFieldLabel(key)
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return ""
		}
		return fmt.Sprintf("%s:\n%s", label, trimmed)
	case []any:
		return projectArraySection(label, v)
	case map[string]any:
		return projectObjectSection(label, v)
	default:
		scalar := stringifyScalar(v)
		if scalar == "" {
			return ""
		}
		return fmt.Sprintf("%s: %s", label, scalar)
	}
}

// projectArraySection renders an array field as a bulleted item list,
// capped at subagentYieldProjectionMaxArrayItems entries.
func projectArraySection(label string, items []any) string {
	if len(items) == 0 {
		return ""
	}
	shown := items
	truncated := false
	if len(shown) > subagentYieldProjectionMaxArrayItems {
		shown = shown[:subagentYieldProjectionMaxArrayItems]
		truncated = true
	}
	lines := make([]string, 0, len(shown)+1)
	for _, item := range shown {
		line := projectArrayItem(item)
		if line == "" {
			continue
		}
		lines = append(lines, "- "+line)
	}
	if len(lines) == 0 {
		return ""
	}
	if truncated {
		lines = append(lines, fmt.Sprintf("- … %d more", len(items)-len(shown)))
	}
	return fmt.Sprintf("%s:\n%s", label, strings.Join(lines, "\n"))
}

// projectObjectSection renders a nested object field as a single line of
// "key: value" pairs, sorted alphabetically for determinism.
func projectObjectSection(label string, obj map[string]any) string {
	if len(obj) == 0 {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		scalar := stringifyScalar(obj[key])
		if scalar == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", key, scalar))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("%s:\n%s", label, strings.Join(parts, "; "))
}

// projectArrayItem renders a single array entry. String items are used
// directly. Object items look for a path/name-like field and a
// description-like field (the common shape for schemas such as Explore's
// "files" array) and join them as "path — description"; objects without
// either fall back to a sorted "key: value" join. Any other scalar is
// stringified.
func projectArrayItem(item any) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		path := firstNonEmptyString(v, []string{"path", "file", "name", "id"})
		desc := firstNonEmptyString(v, []string{"description", "summary", "detail", "note", "reason"})
		switch {
		case path != "" && desc != "":
			return fmt.Sprintf("%s — %s", path, desc)
		case path != "":
			return path
		case desc != "":
			return desc
		default:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, key := range keys {
				scalar := stringifyScalar(v[key])
				if scalar == "" {
					continue
				}
				parts = append(parts, fmt.Sprintf("%s: %s", key, scalar))
			}
			return strings.Join(parts, ", ")
		}
	default:
		return stringifyScalar(v)
	}
}

// firstNonEmptyString returns the first non-blank string value found in obj
// under any of keys, or "" if none match.
func firstNonEmptyString(obj map[string]any, keys []string) string {
	for _, key := range keys {
		if s, ok := obj[key].(string); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// stringifyScalar renders a decoded JSON scalar (string, bool, number) as
// display text. Numbers that are exact integers print without a decimal
// point. Non-scalar values fall back to compact JSON so nothing is silently
// dropped.
func stringifyScalar(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if !math.IsInf(v, 0) && !math.IsNaN(v) && v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// compactJSONFallback re-encodes a decoded payload as compact JSON. Used
// when the payload is not a JSON object, or when none of its fields
// produced readable text -- still strictly better than no content at all.
func compactJSONFallback(decoded any) string {
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// projectionFieldLabel turns a snake_case or kebab-case field name into a
// human label, e.g. "next_actions" -> "Next Actions".
func projectionFieldLabel(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "Field"
	}
	words := strings.FieldsFunc(key, func(r rune) bool {
		return r == '_' || r == '-'
	})
	if len(words) == 0 {
		words = []string{key}
	}
	for i, word := range words {
		if word == "" {
			continue
		}
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// yieldContentText resolves the best available parent-model-facing text
// from a yield result: the model-provided Data if present, otherwise a
// deterministic projection of Payload. Returns "" when both are empty so
// callers can apply their own no-content fallback.
//
// This is the single place that decides "Data wins over Payload, Payload
// projection is the fallback" -- every consumer of a yield result (the
// single-subagent path in runSubAgentDirect, and, via subagentResultText,
// the batch output-metadata and rendering paths) must go through this
// function rather than re-implementing the candidate chain, or the
// candidate selection and the rendered body will drift out of sync (see
// docs/refactor-subagent-result-contract.md §2.1, breakpoints B2-B4).
func yieldContentText(yield message.ToolResultYield) string {
	if text := strings.TrimSpace(yield.Data); text != "" {
		return text
	}
	if len(yield.Payload) > 0 {
		if text := strings.TrimSpace(projectYieldPayload(yield.Payload, nil)); text != "" {
			return text
		}
	}
	return ""
}

// subagentResultText resolves the single candidate chain used everywhere a
// subagent's result must be rendered as parent-model-facing text: explicit
// Content, then the yield's Data/Payload (via yieldContentText), then the
// truncated Preview. All call sites that render a subagentResult for the
// parent model must share this exact chain.
func subagentResultText(result subagentResult) string {
	if content := strings.TrimSpace(result.Content); content != "" {
		return content
	}
	if text := yieldContentText(result.Yield); text != "" {
		return text
	}
	return strings.TrimSpace(result.Preview)
}
