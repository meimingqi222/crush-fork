package styles

import (
	"fmt"
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
)

// ThemeMode selects which palette the UI renders with.
type ThemeMode string

const (
	// ThemeDark always renders the dark palette.
	ThemeDark ThemeMode = "dark"
	// ThemeLight always renders the light palette.
	ThemeLight ThemeMode = "light"
	// ThemeAuto follows the terminal background color, falling back to dark
	// when the terminal does not answer the background query.
	ThemeAuto ThemeMode = "auto"
)

// ParseThemeMode normalizes a configured theme value. Unknown values fall back
// to ThemeAuto so a typo degrades to detection rather than to a wrong palette.
func ParseThemeMode(s string) ThemeMode {
	switch ThemeMode(strings.ToLower(strings.TrimSpace(s))) {
	case ThemeDark:
		return ThemeDark
	case ThemeLight:
		return ThemeLight
	default:
		return ThemeAuto
	}
}

// Palette is the set of semantic colors a theme provides. Styles derives every
// concrete lipgloss style from these, so adding a theme means filling this in
// rather than touching the style tree.
//
// The neutral ramp mirrors between themes. charmtone orders its neutrals
// Pepper → BBQ → Charcoal → Iron → Oyster → Squid → Smoke → Ash → Salt →
// Butter (darkest to lightest); the light palette takes the same positions
// counted from the other end, which keeps relative contrast identical in both
// directions.
type Palette struct {
	// Dark reports whether this palette targets a dark terminal background.
	Dark bool

	// Brand
	Primary   color.Color
	Secondary color.Color
	Tertiary  color.Color

	// Backgrounds, increasing separation from the base surface.
	BgBase        color.Color
	BgBaseLighter color.Color
	BgSubtle      color.Color
	BgOverlay     color.Color

	// Foregrounds, increasing emphasis.
	FgSubtle    color.Color
	FgMuted     color.Color
	FgHalfMuted color.Color
	FgBase      color.Color
	// FgBright is one step past FgBase, for text that must stand out from
	// body copy (headings inside rendered markdown, for example).
	FgBright color.Color

	// Borders
	Border      color.Color
	BorderFocus color.Color

	// Status
	Error   color.Color
	Warning color.Color
	Info    color.Color

	// White is high-contrast text drawn on top of a filled accent color. It
	// stays light in both themes because the fill underneath is saturated.
	White color.Color

	// The two candidates for text drawn on a colored fill. Callers should not
	// pick between them by hand -- use [Palette.OnFill], which chooses by
	// measured contrast against the specific fill.
	FgOnFillLight color.Color
	FgOnFillDark  color.Color

	BlueLight color.Color
	Blue      color.Color
	BlueDark  color.Color

	Yellow color.Color

	GreenLight color.Color
	Green      color.Color
	GreenDark  color.Color

	Red     color.Color
	RedDark color.Color

	// Diff line fills. These are tinted surfaces rather than accents: the
	// code sits on top of them, so a dark theme wants a barely-lifted dark
	// tint and a light theme a barely-dropped light one. Reusing the dark
	// values in light mode is what painted solid black blocks over a white
	// diff.
	DiffInsertFg       color.Color
	DiffInsertGutterBg color.Color
	DiffInsertCodeBg   color.Color
	DiffDeleteFg       color.Color
	DiffDeleteGutterBg color.Color
	DiffDeleteCodeBg   color.Color
}

// DarkPalette is the original Crush palette, unchanged.
func DarkPalette() Palette {
	return Palette{
		Dark: true,

		Primary:   charmtone.Charple,
		Secondary: charmtone.Dolly,
		Tertiary:  charmtone.Bok,

		BgBase:        charmtone.Pepper,
		BgBaseLighter: charmtone.BBQ,
		BgSubtle:      charmtone.Charcoal,
		BgOverlay:     charmtone.Iron,

		FgSubtle:    charmtone.Oyster,
		FgMuted:     charmtone.Squid,
		FgHalfMuted: charmtone.Smoke,
		FgBase:      charmtone.Ash,
		FgBright:    charmtone.Salt,

		Border:      charmtone.Charcoal,
		BorderFocus: charmtone.Charple,

		Error:   charmtone.Sriracha,
		Warning: charmtone.Zest,
		Info:    charmtone.Malibu,

		White:         charmtone.Butter,
		FgOnFillLight: charmtone.Butter,
		FgOnFillDark:  charmtone.Pepper,

		BlueLight: charmtone.Sardine,
		Blue:      charmtone.Malibu,
		BlueDark:  charmtone.Damson,

		Yellow: charmtone.Mustard,

		GreenLight: charmtone.Bok,
		Green:      charmtone.Julep,
		GreenDark:  charmtone.Guac,

		Red:     charmtone.Coral,
		RedDark: charmtone.Sriracha,

		DiffInsertFg:       lipgloss.Color("#629657"),
		DiffInsertGutterBg: lipgloss.Color("#2b322a"),
		DiffInsertCodeBg:   lipgloss.Color("#323931"),
		DiffDeleteFg:       lipgloss.Color("#a45c59"),
		DiffDeleteGutterBg: lipgloss.Color("#312929"),
		DiffDeleteCodeBg:   lipgloss.Color("#383030"),
	}
}

// LightPalette mirrors the dark neutral ramp and swaps the accents that are
// unreadable on a light surface. Charm's neon greens, yellows and cyans are
// tuned for dark backgrounds -- Julep (#00FFB2) and Zest (#E8FE96) fall under
// 1.5:1 against Butter -- so the light palette reaches for the darker sibling
// of the same hue rather than inventing new colors.
func LightPalette() Palette {
	return Palette{
		Dark: false,

		Primary:   charmtone.Charple,
		Secondary: charmtone.Pony,
		// Tertiary tints the editor prompt glyph. Zinc (#10B1AE), the dark
		// theme's teal, only reaches 2.55:1 on Butter; NeueZinc is the same
		// hue a step darker and clears the 3:1 bar for non-text glyphs.
		Tertiary: charmtone.NeueZinc,

		BgBase:        charmtone.Butter,
		BgBaseLighter: charmtone.Salt,
		BgSubtle:      charmtone.Ash,
		BgOverlay:     charmtone.Smoke,

		FgSubtle:    charmtone.Squid,
		FgMuted:     charmtone.Oyster,
		FgHalfMuted: charmtone.Iron,
		FgBase:      charmtone.Charcoal,
		FgBright:    charmtone.Pepper,

		Border:      charmtone.Ash,
		BorderFocus: charmtone.Charple,

		Error:   charmtone.Sriracha,
		Warning: charmtone.Cumin,
		Info:    charmtone.Damson,

		White:         charmtone.Butter,
		FgOnFillLight: charmtone.Butter,
		FgOnFillDark:  charmtone.Pepper,

		BlueLight: charmtone.Malibu,
		Blue:      charmtone.Damson,
		BlueDark:  charmtone.Damson,

		Yellow: charmtone.Cumin,

		GreenLight: charmtone.Guac,
		Green:      charmtone.Guac,
		GreenDark:  charmtone.Pickle,

		Red:     charmtone.Sriracha,
		RedDark: charmtone.Sriracha,

		// Material green/red 50 and 100: the same "tinted paper" role the
		// dark theme gives its near-black greens, mirrored for a light page.
		DiffInsertFg:       lipgloss.Color("#2e7d32"),
		DiffInsertGutterBg: lipgloss.Color("#c8e6c9"),
		DiffInsertCodeBg:   lipgloss.Color("#e8f5e9"),
		DiffDeleteFg:       lipgloss.Color("#c62828"),
		DiffDeleteGutterBg: lipgloss.Color("#ffcdd2"),
		DiffDeleteCodeBg:   lipgloss.Color("#ffebee"),
	}
}

// OnFill returns the text color to draw on top of fill.
//
// Text over a colored fill must contrast with that fill, not with the page.
// Which of the two candidates wins is not a property of the theme but of the
// individual color: white reads better on Charple, black reads better on
// Pony. Picking by category instead of by measurement is what put dark text
// on the light theme's purple selection bar -- and what would have put light
// text on its pink one.
func (p Palette) OnFill(fill color.Color) color.Color {
	if contrast(p.FgOnFillLight, fill) >= contrast(p.FgOnFillDark, fill) {
		return p.FgOnFillLight
	}
	return p.FgOnFillDark
}

// contrast is the WCAG 2.x contrast ratio between two opaque colors.
func contrast(a, b color.Color) float64 {
	l1, l2 := relLuminance(a), relLuminance(b)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

func relLuminance(c color.Color) float64 {
	if c == nil {
		return 0
	}
	r, g, b, _ := c.RGBA()
	f := func(v uint32) float64 {
		s := float64(v) / 65535.0
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*f(r) + 0.7152*f(g) + 0.0722*f(b)
}

// hex renders a color as the "#rrggbb" string the glamour/chroma style
// structs expect. charmtone.Key has its own Hex method, but palette fields are
// plain color.Color so themes can supply colors from any source.
func hex(c color.Color) string {
	if c == nil {
		return "#000000"
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// PaletteFor resolves a configured mode against the detected terminal
// background. terminalIsDark is only consulted for ThemeAuto.
func PaletteFor(mode ThemeMode, terminalIsDark bool) Palette {
	switch mode {
	case ThemeLight:
		return LightPalette()
	case ThemeDark:
		return DarkPalette()
	default:
		if terminalIsDark {
			return DarkPalette()
		}
		return LightPalette()
	}
}
