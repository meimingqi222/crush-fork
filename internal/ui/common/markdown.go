package common

import (
	"reflect"
	"sync"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

type rendererCacheKey struct {
	plain   bool
	width   int
	styleID uintptr
}

const rendererCacheMaxSize = 64

var (
	rendererCache   = make(map[rendererCacheKey]*glamour.TermRenderer)
	rendererCacheMu sync.RWMutex
)

func getCachedRenderer(plain bool, sty *styles.Styles, width int) *glamour.TermRenderer {
	key := rendererCacheKey{
		plain:   plain,
		width:   width,
		styleID: reflect.ValueOf(sty).Pointer(),
	}

	rendererCacheMu.RLock()
	if r, ok := rendererCache[key]; ok {
		rendererCacheMu.RUnlock()
		return r
	}
	rendererCacheMu.RUnlock()

	rendererCacheMu.Lock()
	defer rendererCacheMu.Unlock()
	if r, ok := rendererCache[key]; ok {
		return r
	}

	var r *glamour.TermRenderer
	var err error
	if plain {
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(sty.PlainMarkdown),
			glamour.WithWordWrap(width),
		)
	} else {
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(sty.Markdown),
			glamour.WithWordWrap(width),
		)
	}
	if err != nil {
		return nil
	}
	if len(rendererCache) >= rendererCacheMaxSize {
		clear(rendererCache)
	}
	rendererCache[key] = r
	return r
}

// MarkdownRenderer returns a glamour [glamour.TermRenderer] configured with
// the given styles and width. Results are cached to avoid the expensive
// re-creation of the renderer on every call.
func MarkdownRenderer(sty *styles.Styles, width int) *glamour.TermRenderer {
	return getCachedRenderer(false, sty, width)
}

// PlainMarkdownRenderer returns a glamour [glamour.TermRenderer] with no colors
// (plain text with structure) and the given width. Results are cached to avoid
// the expensive re-creation of the renderer on every call.
func PlainMarkdownRenderer(sty *styles.Styles, width int) *glamour.TermRenderer {
	return getCachedRenderer(true, sty, width)
}
