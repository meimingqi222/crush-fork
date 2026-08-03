package common

import (
	"image/color"
	"reflect"
	"sync"

	"charm.land/glamour/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/xchroma"
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

const formatterName = "crush"

func init() {
	// NOTE: Glamour does not offer us an option to pass the formatter
	// implementation directly. We need to register and use by name.
	var zero color.Color
	formatters.Register(formatterName, xchroma.Formatter(zero, nil))
}

// ResetMarkdownRenderers drops every cached renderer.
//
// The cache is keyed by the *address* of the [styles.Styles] value, so a theme
// switch that rewrites those styles in place leaves the key unchanged and
// would otherwise keep serving renderers built from the previous palette.
// Callers that mutate styles in place must call this afterwards.
func ResetMarkdownRenderers() {
	rendererCacheMu.Lock()
	defer rendererCacheMu.Unlock()
	clear(rendererCache)
}

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
			glamour.WithChromaFormatter(formatterName),
		)
	} else {
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(sty.Markdown),
			glamour.WithWordWrap(width),
			glamour.WithChromaFormatter(formatterName),
		)
	}
	if err != nil {
		return nil
	}
	if len(rendererCache) >= rendererCacheMaxSize {
		// Evict roughly half the entries (pseudo-random via map iteration
		// order) instead of clearing everything. This avoids a "cache
		// cliff" where all renderers must be recreated simultaneously
		// after a single overflow event.
		toRemove := len(rendererCache) / 2
		for k := range rendererCache {
			if toRemove <= 0 {
				break
			}
			delete(rendererCache, k)
			toRemove--
		}
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
