package fantasy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"charm.land/fantasy/jsonrepair"
	"charm.land/fantasy/schema"
	"github.com/kaptinlin/jsonschema"
)

const maxCoercionPasses = 5

var (
	additionalPropertyPattern = regexp.MustCompile(`Additional property '([^']+)'`)
	numericStringPattern      = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?$`)
	jsonNumberPattern         = regexp.MustCompile(`^[+-]?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$`)
)

var compiledValidatorCache sync.Map // map[string]*jsonschema.Schema

type flatIssue struct {
	keyword      string
	instancePath string
	expectedType string
}

// buildToolInputSchema constructs the wire JSON Schema used for validation and
// repair. Root-level additional properties are allowed so hallucinated keys
// can flow through to tools that reject disabled arguments. Nested object
// schemas may still declare additionalProperties: false.
func buildToolInputSchema(toolInfo ToolInfo) map[string]any {
	// Deep clone Parameters to prevent schema.Normalize from mutating the tool's
	// internal state.
	inputSchema := map[string]any{
		"type":       "object",
		"properties": cloneDefaultValue(toolInfo.Parameters),
		"required":   toolInfo.Required,
	}
	schema.Normalize(inputSchema)
	return inputSchema
}

func getCompiledValidator(toolInfo ToolInfo) (*jsonschema.Schema, error) {
	inputSchema := buildToolInputSchema(toolInfo)
	schemaBytes, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schema: %w", err)
	}

	cacheKey := validatorCacheKey(toolInfo.Name, schemaBytes)
	if cached, ok := compiledValidatorCache.Load(cacheKey); ok {
		return cached.(*jsonschema.Schema), nil
	}

	compiler := jsonschema.NewCompiler()
	compiled, err := compiler.Compile(schemaBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}

	actual, _ := compiledValidatorCache.LoadOrStore(cacheKey, compiled)
	return actual.(*jsonschema.Schema), nil
}

func validatorCacheKey(toolName string, schemaBytes []byte) string {
	sum := sha256.Sum256(schemaBytes)
	return toolName + ":" + hex.EncodeToString(sum[:])
}

// validateAndNormalizeToolArguments validates tool arguments against the tool
// schema. It always normalizes optional nulls first, then validates, then
// applies issue-driven coercions for up to maxCoercionPasses rounds.
func validateAndNormalizeToolArguments(toolInfo ToolInfo, originalArgs map[string]any) (map[string]any, error) {
	inputSchema := buildToolInputSchema(toolInfo)
	validator, err := getCompiledValidator(toolInfo)
	if err != nil {
		return nil, err
	}

	normalizedArgs := cloneAnyMap(originalArgs)
	changed := false

	if next, ok := normalizeOptionalNullsForSchema(inputSchema, normalizedArgs, true); ok {
		normalizedArgs = next
		changed = true
	}

	result := validator.Validate(normalizedArgs)
	if result.IsValid() {
		return preserveUnknownRootFields(originalArgs, normalizedArgs, toolInfo.Parameters), nil
	}

	issues := flattenValidationIssues(result)
	for pass := 0; pass < maxCoercionPasses; pass++ {
		coerced, coercionChanged := coerceArgsFromIssues(normalizedArgs, issues)
		if !coercionChanged {
			break
		}
		normalizedArgs = coerced
		changed = true

		if next, ok := normalizeOptionalNullsForSchema(inputSchema, normalizedArgs, true); ok {
			normalizedArgs = next
		}

		result = validator.Validate(normalizedArgs)
		if result.IsValid() {
			return preserveUnknownRootFields(originalArgs, normalizedArgs, toolInfo.Parameters), nil
		}
		issues = flattenValidationIssues(result)
	}

	_ = changed
	return normalizedArgs, formatToolValidationError(toolInfo.Name, originalArgs, normalizedArgs, result)
}

func formatToolValidationError(toolName string, originalArgs, normalizedArgs map[string]any, result *jsonschema.EvaluationResult) error {
	var messages []string
	for path, msg := range result.DetailedErrors() {
		messages = append(messages, fmt.Sprintf("  - %s: %s", formatIssuePath(path), msg))
	}
	if len(messages) == 0 {
		messages = append(messages, "  - root: unknown validation error")
	}

	receivedArgs := any(originalArgs)
	if !mapsEqual(originalArgs, normalizedArgs) {
		receivedArgs = map[string]any{
			"original":   originalArgs,
			"normalized": normalizedArgs,
		}
	}
	receivedJSON, _ := json.MarshalIndent(receivedArgs, "", "  ")

	return fmt.Errorf(
		"validation failed for tool %q:\n%s\n\nReceived arguments:\n%s",
		toolName,
		strings.Join(messages, "\n"),
		receivedJSON,
	)
}

func formatIssuePath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "root"
	}
	return strings.ReplaceAll(path, "/", ".")
}

func flattenValidationIssues(result *jsonschema.EvaluationResult) []flatIssue {
	issues := make([]flatIssue, 0)
	seen := make(map[string]struct{})

	for path, msg := range result.DetailedErrors() {
		issue := flatIssue{keyword: "other", instancePath: path}

		switch {
		case strings.HasSuffix(path, "/type"):
			issue.keyword = "type"
			issue.instancePath = strings.TrimSuffix(path, "/type")
			issue.expectedType = expectedTypeFromMessage(msg)
		case strings.HasSuffix(path, "/schema"):
			issue.keyword = "unrecognized"
			issue.instancePath = strings.TrimSuffix(path, "/schema")
		case strings.HasPrefix(path, "additionalProperties:"):
			issue.keyword = "unrecognized"
			if matches := additionalPropertyPattern.FindStringSubmatch(msg); len(matches) == 2 {
				issue.instancePath = "/" + matches[1]
			}
		}

		key := issue.keyword + "|" + issue.instancePath + "|" + issue.expectedType
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		issues = append(issues, issue)
	}

	return issues
}

func expectedTypeFromMessage(msg string) string {
	const marker = "should be "
	if idx := strings.LastIndex(msg, marker); idx >= 0 {
		return strings.TrimSpace(msg[idx+len(marker):])
	}
	return ""
}

func coerceArgsFromIssues(args map[string]any, issues []flatIssue) (map[string]any, bool) {
	if len(issues) == 0 {
		return args, false
	}

	changed := false
	nextArgs := args

	for _, issue := range issues {
		switch issue.keyword {
		case "unrecognized":
			if issue.instancePath == "" {
				continue
			}
			updated, deleted := deleteValueAtPointer(nextArgs, issue.instancePath)
			if deleted {
				nextArgs = updated
				changed = true
			}
		case "type":
			if issue.expectedType == "" || issue.instancePath == "" {
				continue
			}
			currentValue := getValueAtPointer(nextArgs, issue.instancePath)
			strVal, ok := currentValue.(string)
			if !ok {
				continue
			}
			parsed, parsedOK := tryParseJSONForType(strVal, issue.expectedType)
			if !parsedOK {
				continue
			}
			updated, setOK := setValueAtPointer(nextArgs, issue.instancePath, parsed)
			if setOK {
				nextArgs = updated
				changed = true
			}
		}
	}

	return nextArgs, changed
}

func tryParseJSONForType(value, expectedType string) (any, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, false
	}

	if expectedType == "number" || expectedType == "integer" {
		if !numericStringPattern.MatchString(trimmed) {
			return nil, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || !matchesExpectedType(parsed, expectedType) {
			return nil, false
		}
		return parsed, true
	}

	looksJSONObject := strings.HasPrefix(trimmed, "{")
	looksJSONArray := strings.HasPrefix(trimmed, "[")
	looksJSONLiteral := trimmed == "true" || trimmed == "false" || trimmed == "null" || jsonNumberPattern.MatchString(trimmed)
	if !looksJSONObject && !looksJSONArray && !looksJSONLiteral {
		return nil, false
	}

	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		if matchesExpectedType(parsed, expectedType) {
			return parsed, true
		}
	}

	if looksJSONObject || looksJSONArray {
		if leading := tryParseLeadingJSONContainer(trimmed); leading != nil {
			if matchesExpectedType(leading, expectedType) {
				return leading, true
			}
		}
		if repaired, err := jsonrepair.RepairJSON(trimmed); err == nil {
			if err := json.Unmarshal([]byte(repaired), &parsed); err == nil {
				if matchesExpectedType(parsed, expectedType) {
					return parsed, true
				}
			}
		}
	}

	return nil, false
}

func matchesExpectedType(value any, expectedType string) bool {
	switch expectedType {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		f, ok := value.(float64)
		return ok && !isNaN(f)
	case "integer":
		f, ok := value.(float64)
		return ok && !isNaN(f) && f == float64(int64(f))
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

func isNaN(f float64) bool {
	return f != f
}

func tryParseLeadingJSONContainer(value string) any {
	if len(value) == 0 {
		return nil
	}
	firstChar := value[0]
	var closingChar byte
	switch firstChar {
	case '{':
		closingChar = '}'
	case '[':
		closingChar = ']'
	default:
		return nil
	}

	depth := 0
	inString := false
	escaped := false

	for i := 0; i < len(value); i++ {
		ch := value[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if ch == firstChar {
			depth++
			continue
		}
		if ch != closingChar {
			continue
		}
		depth--
		if depth != 0 {
			continue
		}

		prefix := value[:i+1]
		var parsed any
		if err := json.Unmarshal([]byte(prefix), &parsed); err == nil {
			return parsed
		}
		if repaired, err := jsonrepair.RepairJSON(prefix); err == nil {
			if err := json.Unmarshal([]byte(repaired), &parsed); err == nil {
				return parsed
			}
		}
	}

	return nil
}

// normalizeOptionalNullsForSchema removes null/"null" from optional fields,
// substitutes schema defaults for required nullish fields, recursively
// normalizes nested values, and strips unknown nullish keys when extras are
// forbidden at non-root object schemas.
func normalizeOptionalNullsForSchema(schemaNode map[string]any, value map[string]any, isRoot bool) (map[string]any, bool) {
	if len(schemaNode) == 0 || len(value) == 0 {
		return value, false
	}

	if coerced, ok := normalizeValueForSchema(schemaNode, value, isRoot); ok {
		if next, ok := coerced.(map[string]any); ok {
			return next, true
		}
	}
	return value, false
}

func normalizeValueForSchema(schemaNode map[string]any, value any, isRoot bool) (any, bool) {
	if value == nil {
		return value, false
	}

	if anyOf, ok := schemaNode["anyOf"].([]any); ok {
		return normalizeAnyOfLike(anyOf, value, isRoot)
	}
	if oneOf, ok := schemaNode["oneOf"].([]any); ok {
		return normalizeAnyOfLike(oneOf, value, isRoot)
	}
	if allOf, ok := schemaNode["allOf"].([]any); ok {
		changed := false
		nextValue := value
		for _, branch := range allOf {
			branchSchema, ok := branch.(map[string]any)
			if !ok {
				continue
			}
			if normalized, branchChanged := normalizeValueForSchema(branchSchema, nextValue, isRoot); branchChanged {
				nextValue = normalized
				changed = true
			}
		}
		if changed {
			return nextValue, true
		}
		return value, false
	}

	schemaType, _ := schemaNode["type"].(string)
	if schemaType == "number" || schemaType == "integer" {
		if strVal, ok := value.(string); ok {
			if parsed, ok := tryParseJSONForType(strVal, schemaType); ok {
				return parsed, true
			}
		}
	}

	if schemaType != "" && schemaType != "object" {
		return value, false
	}

	if items, ok := schemaNode["items"].(map[string]any); ok {
		arr, ok := value.([]any)
		if !ok {
			return value, false
		}
		changed := false
		next := make([]any, len(arr))
		copy(next, arr)
		for i, item := range arr {
			if normalized, itemChanged := normalizeValueForSchema(items, item, false); itemChanged {
				next[i] = normalized
				changed = true
			}
		}
		if changed {
			return next, true
		}
		return value, false
	}

	obj, ok := value.(map[string]any)
	if !ok {
		return value, false
	}

	properties, ok := schemaNode["properties"].(map[string]any)
	if !ok {
		return value, false
	}

	requiredSet := make(map[string]bool)
	switch req := schemaNode["required"].(type) {
	case []string:
		for _, name := range req {
			requiredSet[name] = true
		}
	case []any:
		for _, item := range req {
			if name, ok := item.(string); ok {
				requiredSet[name] = true
			}
		}
	}

	changed := false
	nextValue := obj

	for key, currentValue := range obj {
		propertySchema, ok := properties[key].(map[string]any)
		if !ok {
			continue
		}

		if isNullish(currentValue) {
			if !requiredSet[key] {
				if !changed {
					nextValue = cloneAnyMap(obj)
					changed = true
				}
				delete(nextValue, key)
				continue
			}
			if def, hasDefault := propertySchema["default"]; hasDefault {
				if !changed {
					nextValue = cloneAnyMap(obj)
					changed = true
				}
				nextValue[key] = cloneDefaultValue(def)
				continue
			}
		}

		if normalized, propertyChanged := normalizeValueForSchema(propertySchema, currentValue, false); propertyChanged {
			if !changed {
				nextValue = cloneAnyMap(obj)
				changed = true
			}
			nextValue[key] = normalized
		}
	}

	if !isRoot && schemaNode["additionalProperties"] == false {
		knownKeys := make(map[string]struct{}, len(properties))
		for key := range properties {
			knownKeys[key] = struct{}{}
		}
		for key, currentValue := range nextValue {
			if _, known := knownKeys[key]; known {
				continue
			}
			if !isNullish(currentValue) {
				continue
			}
			if !changed {
				nextValue = cloneAnyMap(nextValue)
				changed = true
			}
			delete(nextValue, key)
		}
	}

	if changed {
		return nextValue, true
	}
	return value, false
}

func normalizeAnyOfLike(branches []any, value any, isRoot bool) (any, bool) {
	var changedCandidate any
	hasChangedCandidate := false

	for _, branch := range branches {
		branchSchema, ok := branch.(map[string]any)
		if !ok {
			continue
		}
		normalized, changed := normalizeValueForSchema(branchSchema, value, isRoot)
		if !changed {
			continue
		}
		changedCandidate = normalized
		hasChangedCandidate = true
	}

	if hasChangedCandidate {
		return changedCandidate, true
	}
	return value, false
}

func isNullish(value any) bool {
	if value == nil {
		return true
	}
	str, ok := value.(string)
	return ok && str == "null"
}

func preserveUnknownRootFields(original, normalized, knownProperties map[string]any) map[string]any {
	result := cloneAnyMap(normalized)
	for key, value := range original {
		if _, known := knownProperties[key]; known {
			continue
		}
		result[key] = value
	}
	return result
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for key, val := range a {
		other, ok := b[key]
		if !ok {
			return false
		}
		aJSON, errA := json.Marshal(val)
		bJSON, errB := json.Marshal(other)
		if errA != nil || errB != nil {
			return false
		}
		if string(aJSON) != string(bJSON) {
			return false
		}
	}
	return true
}

func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, value := range m {
		out[key] = value
	}
	return out
}

func cloneDefaultValue(value any) any {
	cloned, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal value for cloning: %v", err))
	}
	var out any
	if err := json.Unmarshal(cloned, &out); err != nil {
		panic(fmt.Sprintf("failed to unmarshal cloned value: %v", err))
	}
	return out
}

func getValueAtPointer(root map[string]any, pointer string) any {
	segments := decodeJSONPointer(pointer)
	if len(segments) == 0 {
		return root
	}
	current := any(root)
	for _, segment := range segments {
		switch node := current.(type) {
		case map[string]any:
			current = node[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nil
			}
			current = node[index]
		default:
			return nil
		}
	}
	return current
}

func setValueAtPointer(root map[string]any, pointer string, value any) (map[string]any, bool) {
	segments := decodeJSONPointer(pointer)
	if len(segments) == 0 {
		return nil, false
	}

	nextRoot := cloneAnyMap(root)
	current := any(nextRoot)

	for i, segment := range segments {
		isLeaf := i == len(segments)-1
		switch node := current.(type) {
		case map[string]any:
			if isLeaf {
				node[segment] = value
				return nextRoot, true
			}
			child, ok := node[segment]
			if !ok {
				return nextRoot, false
			}
			switch child.(type) {
			case map[string]any:
				clonedChild := cloneAnyMap(child.(map[string]any))
				node[segment] = clonedChild
				current = clonedChild
			case []any:
				clonedChild := cloneAnySlice(child.([]any))
				node[segment] = clonedChild
				current = clonedChild
			default:
				return nextRoot, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nextRoot, false
			}
			if isLeaf {
				node[index] = value
				return nextRoot, true
			}
			switch child := node[index].(type) {
			case map[string]any:
				clonedChild := cloneAnyMap(child)
				node[index] = clonedChild
				current = clonedChild
			case []any:
				clonedChild := cloneAnySlice(child)
				node[index] = clonedChild
				current = clonedChild
			default:
				return nextRoot, false
			}
		default:
			return nextRoot, false
		}
	}

	return nextRoot, false
}

func deleteValueAtPointer(root map[string]any, pointer string) (map[string]any, bool) {
	segments := decodeJSONPointer(pointer)
	if len(segments) == 0 {
		return root, false
	}

	nextRoot := cloneAnyMap(root)
	current := any(nextRoot)

	for i, segment := range segments {
		isLeaf := i == len(segments)-1
		switch node := current.(type) {
		case map[string]any:
			if isLeaf {
				if _, exists := node[segment]; !exists {
					return nextRoot, false
				}
				delete(node, segment)
				return nextRoot, true
			}
			child, ok := node[segment]
			if !ok {
				return nextRoot, false
			}
			switch child.(type) {
			case map[string]any:
				clonedChild := cloneAnyMap(child.(map[string]any))
				node[segment] = clonedChild
				current = clonedChild
			case []any:
				clonedChild := cloneAnySlice(child.([]any))
				node[segment] = clonedChild
				current = clonedChild
			default:
				return nextRoot, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return nextRoot, false
			}
			if isLeaf {
				updated := append(append([]any{}, node[:index]...), node[index+1:]...)
				if !replacePointerParent(nextRoot, segments[:i], updated) {
					return nextRoot, false
				}
				return nextRoot, true
			}
			switch child := node[index].(type) {
			case map[string]any:
				clonedChild := cloneAnyMap(child)
				node[index] = clonedChild
				current = clonedChild
			case []any:
				clonedChild := cloneAnySlice(child)
				node[index] = clonedChild
				current = clonedChild
			default:
				return nextRoot, false
			}
		default:
			return nextRoot, false
		}
	}

	return nextRoot, false
}

func decodeJSONPointer(pointer string) []string {
	pointer = strings.TrimPrefix(pointer, "/")
	if pointer == "" {
		return nil
	}
	return strings.Split(pointer, "/")
}

func cloneAnySlice(values []any) []any {
	out := make([]any, len(values))
	copy(out, values)
	return out
}

func replacePointerParent(root map[string]any, parentSegments []string, value any) bool {
	if len(parentSegments) == 0 {
		return false
	}

	current := any(root)
	for i, segment := range parentSegments {
		isLeaf := i == len(parentSegments)-1
		switch node := current.(type) {
		case map[string]any:
			if isLeaf {
				node[segment] = value
				return true
			}
			child, ok := node[segment]
			if !ok {
				return false
			}
			current = child
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return false
			}
			if isLeaf {
				node[index] = value
				return true
			}
			current = node[index]
		default:
			return false
		}
	}
	return false
}

// repairToolArguments applies the built-in validation/repair pipeline to a tool
// call that already failed initial validation.
func repairToolArguments(options ToolCallRepairOptions) (*ToolCallContent, error) {
	var toolInfo *ToolInfo
	for _, t := range options.AvailableTools {
		info := t.Info()
		if info.Name == options.OriginalToolCall.ToolName {
			toolInfo = &info
			break
		}
	}
	if toolInfo == nil {
		return nil, nil
	}

	var originalArgs map[string]any
	if err := json.Unmarshal([]byte(options.OriginalToolCall.Input), &originalArgs); err != nil {
		return nil, err
	}

	normalized, err := validateAndNormalizeToolArguments(*toolInfo, originalArgs)
	if err != nil {
		return nil, nil
	}

	if mapsEqual(originalArgs, normalized) {
		return nil, nil
	}

	repairedInput, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}

	repaired := options.OriginalToolCall
	repaired.Input = string(repairedInput)
	return &repaired, nil
}
