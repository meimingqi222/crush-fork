package common

import (
	"bytes"
	"hash/fnv"
	"image/color"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// --- Style cache (keyed by background colour) ---

type bgColourKey [3]uint8

var (
	styleCache   = make(map[bgColourKey]*chroma.Style)
	styleCacheMu sync.RWMutex
)

func makeBgKey(bg color.Color) bgColourKey {
	r, g, b, _ := bg.RGBA()
	return bgColourKey{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

func getCachedStyle(st *styles.Styles, bg color.Color) *chroma.Style {
	key := makeBgKey(bg)

	styleCacheMu.RLock()
	if s, ok := styleCache[key]; ok {
		styleCacheMu.RUnlock()
		return s
	}
	styleCacheMu.RUnlock()

	styleCacheMu.Lock()
	defer styleCacheMu.Unlock()
	if s, ok := styleCache[key]; ok {
		return s
	}

	base := chroma.MustNewStyle("crush", st.ChromaTheme())
	r, g, b, _ := bg.RGBA()
	s, err := base.Builder().Transform(
		func(t chroma.StyleEntry) chroma.StyleEntry {
			t.Background = chroma.NewColour(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			return t
		},
	).Build()
	if err != nil {
		s = chromastyles.Fallback
	}
	styleCache[key] = s
	return s
}

// --- Full-result cache (keyed by source hash + fileName + bg colour) ---

type highlightResult struct {
	source   string // kept for collision detection
	fileName string
	result   string
}

type highlightCacheKey struct {
	srcHash  uint64
	fileName string
	bg       bgColourKey
}

const highlightCacheMaxSize = 256

var (
	highlightCache   = make(map[highlightCacheKey]highlightResult)
	highlightCacheMu sync.RWMutex
)

func hashSource(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func getCachedHighlight(key highlightCacheKey, source, fileName string) (string, bool) {
	highlightCacheMu.RLock()
	e, ok := highlightCache[key]
	highlightCacheMu.RUnlock()
	if !ok || e.source != source || e.fileName != fileName {
		return "", false
	}
	return e.result, true
}

func putCachedHighlight(key highlightCacheKey, entry highlightResult) {
	highlightCacheMu.Lock()
	defer highlightCacheMu.Unlock()
	// Double-check after acquiring write lock.
	if e, ok := highlightCache[key]; ok && e.source == entry.source && e.fileName == entry.fileName {
		return
	}
	if len(highlightCache) >= highlightCacheMaxSize {
		toRemove := len(highlightCache) / 2
		for k := range highlightCache {
			if toRemove <= 0 {
				break
			}
			delete(highlightCache, k)
			toRemove--
		}
	}
	highlightCache[key] = entry
}

// SyntaxHighlight applies syntax highlighting to the given source code based
// on the file name and background color. It returns the highlighted code as a
// string.
func SyntaxHighlight(st *styles.Styles, source, fileName string, bg color.Color) (string, error) {
	bgKey := makeBgKey(bg)

	// Fast path: return a fully cached result when source+file+bg match.
	resultKey := highlightCacheKey{
		srcHash:  hashSource(source),
		fileName: fileName,
		bg:       bgKey,
	}
	if cached, ok := getCachedHighlight(resultKey, source, fileName); ok {
		return cached, nil
	}

	// Determine the language lexer to use
	l := lexers.Match(fileName)
	if l == nil {
		l = lexers.Analyse(source)
	}
	if l == nil {
		l = lexers.Fallback
	}
	l = chroma.Coalesce(l)

	// Get the formatter
	f := formatters.Get("terminal16m")
	if f == nil {
		f = formatters.Fallback
	}

	// Retrieve (or build) the cached chroma style for this background colour.
	s := getCachedStyle(st, bg)

	// Tokenize and format
	it, err := l.Tokenise(nil, source)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	err = f.Format(&buf, s, it)
	result := buf.String()

	// Cache the result for subsequent identical calls.
	if err == nil {
		putCachedHighlight(resultKey, highlightResult{
			source:   source,
			fileName: fileName,
			result:   result,
		})
	}

	return result, err
}
