package httpext

import "encoding/json"

func responsesInputLen(input any) int {
	switch v := input.(type) {
	case []any:
		return len(v)
	default:
		return 0
	}
}

// trimChainedInput returns only the input items appended since the last chained
// request when the current input extends the previous prefix.
func trimChainedInput(input any, previousLen int) any {
	if previousLen <= 0 {
		return input
	}
	items, ok := input.([]any)
	if !ok || len(items) <= previousLen {
		return input
	}
	return items[previousLen:]
}

func inputJSONPrefixEqual(a, b []any, n int) bool {
	if n <= 0 {
		return true
	}
	if len(a) < n || len(b) < n {
		return false
	}
	ab, err := json.Marshal(a[:n])
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b[:n])
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}