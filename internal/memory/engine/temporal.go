package engine

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TemporalExpr represents a parsed time reference from a natural language query.
type TemporalExpr struct {
	// After is the earliest time that matches the expression (inclusive).
	After time.Time
	// Before is the latest time that matches the expression (exclusive).
	Before time.Time
	// Raw is the original text that was parsed.
	Raw string
}

// ParseTemporalExprs extracts time references from a natural language query.
// It supports common English and Chinese temporal expressions such as
// "yesterday", "last week", "3 days ago", "上周", "最近3天", etc.
func ParseTemporalExprs(query string, now time.Time) []TemporalExpr {
	var exprs []TemporalExpr
	lower := strings.ToLower(query)

	patterns := []struct {
		regex *regexp.Regexp
		parse func(match []string, now time.Time) (after, before time.Time)
		lang  string // "en" or "zh"
	}{
		// English: "X days ago", "X hours ago", "X weeks ago", "X months ago"
		{
			regex: regexp.MustCompile(`(\d+)\s+(days?|hours?|weeks?|months?)\s+ago`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				n, _ := strconv.Atoi(match[1])
				unit := match[2]
				return shiftTime(now, n, unit), now
			},
			lang: "en",
		},
		// English: "yesterday"
		{
			regex: regexp.MustCompile(`yesterday`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				y := now.AddDate(0, 0, -1)
				return startOfDay(y), startOfDay(now)
			},
			lang: "en",
		},
		// English: "last week"
		{
			regex: regexp.MustCompile(`last\s+week`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				return startOfWeek(now).AddDate(0, 0, -7), startOfWeek(now)
			},
			lang: "en",
		},
		// English: "this week"
		{
			regex: regexp.MustCompile(`this\s+week`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				return startOfWeek(now), now
			},
			lang: "en",
		},
		// English: "recently", "lately"
		{
			regex: regexp.MustCompile(`recently|lately`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				return now.AddDate(0, 0, -7), now
			},
			lang: "en",
		},
		// Chinese: "最近X天", "最近X个星期", "最近X个月"
		{
			regex: regexp.MustCompile(`最近(\d+)(天|个?星期|周|个月?)`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				n, _ := strconv.Atoi(match[1])
				unit := match[2]
				switch unit {
				case "天":
					return now.AddDate(0, 0, -n), now
				case "星期", "个星期", "周":
					return now.AddDate(0, 0, -7*n), now
				case "个月", "月":
					return now.AddDate(0, -n, 0), now
				default:
					return now.AddDate(0, 0, -n), now
				}
			},
			lang: "zh",
		},
		// Chinese: "昨天"
		{
			regex: regexp.MustCompile(`昨天`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				y := now.AddDate(0, 0, -1)
				return startOfDay(y), startOfDay(now)
			},
			lang: "zh",
		},
		// Chinese: "上周"
		{
			regex: regexp.MustCompile(`上周|上星期`),
			parse: func(match []string, now time.Time) (time.Time, time.Time) {
				return startOfWeek(now).AddDate(0, 0, -7), startOfWeek(now)
			},
			lang: "zh",
		},
	}

	for _, p := range patterns {
		matches := p.regex.FindAllStringSubmatch(lower, -1)
		for _, m := range matches {
			after, before := p.parse(m, now)
			raw := m[0]
			exprs = append(exprs, TemporalExpr{
				After:  after,
				Before: before,
				Raw:    raw,
			})
		}
	}

	return exprs
}

// shiftTime returns the time that is n units before now.
func shiftTime(now time.Time, n int, unit string) time.Time {
	switch strings.TrimSuffix(unit, "s") {
	case "day":
		return now.AddDate(0, 0, -n)
	case "hour":
		return now.Add(-time.Duration(n) * time.Hour)
	case "week":
		return now.AddDate(0, 0, -7*n)
	case "month":
		return now.AddDate(0, -n, 0)
	default:
		return now.AddDate(0, 0, -n)
	}
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func startOfWeek(t time.Time) time.Time {
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	return startOfDay(t.AddDate(0, 0, -(weekday - 1)))
}

// ReciprocalRankFusion merges multiple ranked lists into a single ranking
// using the RRF algorithm.  Each list's rank positions contribute a score
// of 1/(k + rank) where k is a smoothing constant (default 60).
// This is useful for combining FTS5 and vector search results.
func ReciprocalRankFusion(lists [][]MemoryEvent, k int64) []MemoryEvent {
	if k <= 0 {
		k = 60
	}
	if len(lists) == 0 {
		return nil
	}

	scores := make(map[string]float64)
	events := make(map[string]MemoryEvent)

	for _, list := range lists {
		for rank, evt := range list {
			id := evt.ID
			scores[id] += 1.0 / float64(k+int64(rank+1))
			if _, exists := events[id]; !exists {
				events[id] = evt
			}
		}
	}

	// Sort by RRF score descending.
	type scored struct {
		id    string
		score float64
	}
	scoredList := make([]scored, 0, len(scores))
	for id, score := range scores {
		scoredList = append(scoredList, scored{id: id, score: score})
	}

	// Simple insertion sort (small N).
	for i := 1; i < len(scoredList); i++ {
		for j := i; j > 0 && scoredList[j].score > scoredList[j-1].score; j-- {
			scoredList[j], scoredList[j-1] = scoredList[j-1], scoredList[j]
		}
	}

	result := make([]MemoryEvent, 0, len(scoredList))
	for _, s := range scoredList {
		result = append(result, events[s.id])
	}
	return result
}
