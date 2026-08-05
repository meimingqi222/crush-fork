package redact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

const (
	maxCacheSize     = 2000
	maxCacheEntryLen = 512_000
)

var unicodeTagsRe = regexp.MustCompile(`[\x{DB40}][\x{DC00}-\x{DC7F}]`)

func compilePattern(entry *SecretPattern) *regexp.Regexp {
	if entry.compiled != nil {
		return entry.compiled
	}
	re, err := regexp.Compile(entry.Pattern)
	if err != nil {
		slog.Warn("Error compiling regex", "pattern", entry.ID, "error", err)
		re = regexp.MustCompile(`^$`)
	}
	entry.compiled = re
	return re
}

func stripInvisibleUnicode(input string) string {
	stripped := unicodeTagsRe.ReplaceAllString(input, "")
	if len(stripped) != len(input) {
		slog.Info("Invisible Unicode tag characters removed during sanitization",
			"removedCount", len(input)-len(stripped))
	}
	return stripped
}

func RedactString(input string, patterns []SecretPattern, cache map[string]string) string {
	if input == "" {
		return input
	}

	if cache != nil {
		if cached, ok := cache[input]; ok {
			return cached
		}
	}

	result := stripInvisibleUnicode(input)

	for i := range patterns {
		entry := &patterns[i]
		if !keywordMatch(result, entry) {
			continue
		}

		re := compilePattern(entry)
		result = re.ReplaceAllStringFunc(result, func(full string) string {
			submatch := re.FindStringSubmatch(full)
			if len(submatch) > 1 && submatch[1] != "" {
				if len(entry.ExcludeWords) > 0 && containsAnyWord(submatch[1], entry.ExcludeWords) {
					return full
				}
				return strings.Replace(full, submatch[1], fmt.Sprintf("[REDACTED:%s]", entry.ID), 1)
			}
			return full
		})
	}

	if cache != nil && len(input) <= maxCacheEntryLen {
		if len(cache) >= maxCacheSize {
			for k := range cache {
				delete(cache, k)
			}
		}
		cache[input] = result
	}

	return result
}

func keywordMatch(text string, entry *SecretPattern) bool {
	for _, kw := range entry.Keywords {
		if entry.CaseInsensitive {
			if strings.Contains(strings.ToLower(text), strings.ToLower(kw)) {
				return true
			}
		} else {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

func containsAnyWord(value string, words []string) bool {
	lower := strings.ToLower(value)
	for _, w := range words {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func RedactDeep(value interface{}, patterns []SecretPattern, cache map[string]string) interface{} {
	switch v := value.(type) {
	case nil:
		return nil
	case bool:
		return v
	case float64:
		return v
	case string:
		return RedactString(v, patterns, cache)
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			if s, ok := item.(string); ok {
				result[i] = RedactString(s, patterns, cache)
			} else if m, ok := item.(map[string]interface{}); ok {
				result[i] = RedactDeep(m, patterns, cache)
			} else if arr, ok := item.([]interface{}); ok {
				result[i] = RedactDeep(arr, patterns, cache)
			} else {
				result[i] = item
			}
		}
		return result
	case map[string]interface{}:
		if typ, ok := v["type"].(string); ok && (typ == "base64" || typ == "image") {
			if _, hasData := v["data"]; hasData {
				cp := make(map[string]interface{}, len(v))
				for k, val := range v {
					cp[k] = val
				}
				return cp
			}
		}

		if isImg, ok := v["isImage"].(bool); ok && isImg {
			if _, hasContent := v["content"]; hasContent {
				result := make(map[string]interface{}, len(v))
				for k, val := range v {
					if k == "content" {
						result[k] = val
					} else {
						result[k] = RedactDeep(val, patterns, cache)
					}
				}
				return result
			}
		}

		result := make(map[string]interface{}, len(v))
		for k, val := range v {
			result[k] = RedactDeep(val, patterns, cache)
		}
		return result
	}
	return value
}

func RedactToolInput(input string, patterns []SecretPattern, cache map[string]string) string {
	var parsed interface{}
	if err := json.Unmarshal([]byte(input), &parsed); err == nil {
		redacted := RedactDeep(parsed, patterns, cache)
		b, err := json.Marshal(redacted)
		if err == nil {
			return string(b)
		}
	}
	return RedactString(input, patterns, cache)
}
