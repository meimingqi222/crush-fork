package engine

import (
	"strings"
	"unicode"
)

var memoryQueryExpansionGroups = [][]string{
	{"memory", "memories", "recall", "remember", "记忆", "回忆", "召回"},
	{"compaction", "compact", "summary", "summarize", "compression", "压缩", "摘要"},
	{"materialize", "materialized", "materialization", "artifact", "artifacts", "物化", "产物"},
	{"consolidate", "consolidated", "consolidation", "merge", "merged", "合并", "整合"},
	{"extract", "extracted", "extraction", "提取", "抽取"},
	{"preference", "preferences", "prefer", "偏好", "喜好"},
	{"decision", "decisions", "decide", "chosen", "choice", "决策", "决定", "选择"},
	{"pitfall", "pitfalls", "gotcha", "bug", "bugs", "陷阱", "坑", "问题"},
	{"procedure", "procedures", "workflow", "workflows", "process", "流程", "步骤"},
	{"local", "offline", "本地", "离线"},
	{"remote", "hindsight", "远程"},
	{"sqlite", "sqlite3", "database", "db", "数据库"},
}

var memoryQueryExpansionIndex = buildMemoryQueryExpansionIndex()

func buildMemoryQueryExpansionIndex() map[string][]string {
	index := make(map[string][]string)
	for _, group := range memoryQueryExpansionGroups {
		for _, term := range group {
			term = strings.ToLower(strings.TrimSpace(term))
			if term == "" {
				continue
			}
			index[term] = group
		}
	}
	return index
}

func queryTerms(query string) []string {
	return expandedQueryTerms(query)
}

func expandedQueryTerms(query string) []string {
	base := rawQueryTerms(query)
	seen := make(map[string]struct{}, len(base)*2)
	terms := make([]string, 0, len(base)*2)
	add := func(term string) {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			return
		}
		if _, ok := seen[term]; ok {
			return
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	for _, term := range base {
		add(term)
		if group, ok := memoryQueryExpansionIndex[term]; ok {
			for _, synonym := range group {
				add(synonym)
			}
		}
	}
	return terms
}

func rawQueryTerms(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	seen := make(map[string]struct{})
	terms := make([]string, 0, 16)
	add := func(term string) {
		term = strings.ToLower(strings.TrimSpace(term))
		if len([]rune(term)) < 2 && !isCJKToken(term) {
			return
		}
		if _, ok := seen[term]; ok {
			return
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}

	var latin strings.Builder
	var cjk []rune
	flushLatin := func() {
		if latin.Len() == 0 {
			return
		}
		for _, part := range splitIdentifier(latin.String()) {
			add(part)
		}
		latin.Reset()
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		for _, r := range cjk {
			add(string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			add(string(cjk[i : i+2]))
		}
		if len(cjk) > 2 {
			add(string(cjk))
		}
		cjk = cjk[:0]
	}

	for _, r := range query {
		switch {
		case isCJKRune(r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' || r == '.':
			flushCJK()
			latin.WriteRune(r)
		default:
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return terms
}

func splitIdentifier(value string) []string {
	value = strings.Trim(value, "_-. ")
	if value == "" {
		return nil
	}
	var parts []string
	var current strings.Builder
	var previous rune
	flush := func() {
		if current.Len() == 0 {
			return
		}
		parts = append(parts, current.String())
		current.Reset()
	}
	for _, r := range value {
		if r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) {
			flush()
			previous = 0
			continue
		}
		if previous != 0 && unicode.IsLower(previous) && unicode.IsUpper(r) {
			flush()
		}
		current.WriteRune(unicode.ToLower(r))
		previous = r
	}
	flush()
	if len(parts) == 0 {
		return []string{value}
	}
	if len(parts) > 1 {
		parts = append(parts, strings.ToLower(value))
	}
	return parts
}

func isCJKToken(token string) bool {
	runes := []rune(token)
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes {
		if !isCJKRune(r) {
			return false
		}
	}
	return true
}

func isCJKRune(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF)
}
