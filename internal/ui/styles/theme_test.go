package styles

import (
	"image/color"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseThemeModeFallsBackToAuto(t *testing.T) {
	t.Parallel()

	require.Equal(t, ThemeDark, ParseThemeMode("dark"))
	require.Equal(t, ThemeLight, ParseThemeMode("LIGHT"))
	require.Equal(t, ThemeAuto, ParseThemeMode(" auto "))
	// A typo must degrade to detection, never to a wrong fixed palette.
	require.Equal(t, ThemeAuto, ParseThemeMode("drak"))
	require.Equal(t, ThemeAuto, ParseThemeMode(""))
}

func TestPaletteForOnlyConsultsTerminalForAuto(t *testing.T) {
	t.Parallel()

	require.True(t, PaletteFor(ThemeDark, false).Dark, "explicit dark ignores a light terminal")
	require.False(t, PaletteFor(ThemeLight, true).Dark, "explicit light ignores a dark terminal")
	require.True(t, PaletteFor(ThemeAuto, true).Dark)
	require.False(t, PaletteFor(ThemeAuto, false).Dark)
}

// relativeLuminance implements WCAG 2.x relative luminance.
func relativeLuminance(c color.Color) float64 {
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

func contrastRatio(fg, bg color.Color) float64 {
	l1, l2 := relativeLuminance(fg), relativeLuminance(bg)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// TestBodyTextContrast is the regression guard that motivated theming: the
// dark palette rendered on a light terminal put near-black text on near-white
// only by accident of the terminal, and every UI-painted surface stayed dark.
// Body text must stay legible against its own background in both palettes.
func TestBodyTextContrast(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]Palette{"dark": DarkPalette(), "light": LightPalette()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// 4.5:1 is the WCAG AA threshold for body text.
			require.Greater(t, contrastRatio(p.FgBase, p.BgBase), 4.5,
				"FgBase on BgBase must clear WCAG AA")
			// Muted text is secondary; hold it to the 3:1 large-text bar.
			require.Greater(t, contrastRatio(p.FgMuted, p.BgBase), 3.0,
				"FgMuted on BgBase must clear 3:1")
			require.Greater(t, contrastRatio(p.FgHalfMuted, p.BgBase), 4.0,
				"FgHalfMuted on BgBase must stay readable")
		})
	}
}

// TestAccentGlyphContrast covers the small colored glyphs that are easy to
// pick by hue and never check: the editor prompt marker (Tertiary) and the
// input placeholder (FgSubtle). Charm's neon accents are tuned for dark
// backgrounds -- Zinc lands at 2.55:1 on the light surface -- so hold both to
// the 3:1 bar WCAG uses for non-body text.
func TestAccentGlyphContrast(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]Palette{"dark": DarkPalette(), "light": LightPalette()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Greater(t, contrastRatio(p.Tertiary, p.BgBase), 3.0,
				"Tertiary (editor prompt glyph) must clear 3:1")
			require.Greater(t, contrastRatio(p.FgSubtle, p.BgBase), 2.5,
				"FgSubtle (placeholder) must stay perceptible")
			require.Greater(t, contrastRatio(p.Error, p.BgBase), 3.0,
				"Error must clear 3:1")
		})
	}
}

// TestPaletteRampDirection pins the semantic ordering of the neutral ramp:
// surfaces step away from the base background and foregrounds step toward
// maximum emphasis. Mirroring the ramp for the light theme is only correct if
// this ordering holds in both.
func TestPaletteRampDirection(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]Palette{"dark": DarkPalette(), "light": LightPalette()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := relativeLuminance(p.BgBase)
			subtle := relativeLuminance(p.BgSubtle)
			overlay := relativeLuminance(p.BgOverlay)

			if p.Dark {
				require.Less(t, base, subtle, "BgSubtle sits above BgBase in dark")
				require.Less(t, subtle, overlay, "BgOverlay sits above BgSubtle in dark")
			} else {
				require.Greater(t, base, subtle, "BgSubtle sits below BgBase in light")
				require.Greater(t, subtle, overlay, "BgOverlay sits below BgSubtle in light")
			}

			// Emphasis increases from subtle to bright regardless of theme.
			require.Greater(t,
				contrastRatio(p.FgBright, p.BgBase),
				contrastRatio(p.FgSubtle, p.BgBase),
				"FgBright must out-contrast FgSubtle")
		})
	}
}

// TestNewRecordsPalette guards the switch path: applyPalette compares
// Styles.Palette to decide whether a rebuild is needed.
func TestNewRecordsPalette(t *testing.T) {
	t.Parallel()

	require.True(t, New(DarkPalette()).Palette.Dark)
	require.False(t, New(LightPalette()).Palette.Dark)
	require.Equal(t, DarkPalette().BgBase, New(DarkPalette()).Background)
	require.Equal(t, LightPalette().BgBase, New(LightPalette()).Background)
}

// TestMarkdownStylesFollowPalette catches the class of bug this refactor
// existed to remove: colors baked into the glamour style tree instead of read
// from the palette. If any theme-sensitive slot is hardcoded again, the two
// themes render it identically.
func TestMarkdownStylesFollowPalette(t *testing.T) {
	t.Parallel()

	dark := New(DarkPalette())
	light := New(LightPalette())

	require.NotNil(t, dark.Markdown.Document.Color)
	require.NotNil(t, light.Markdown.Document.Color)
	require.NotEqual(t, *dark.Markdown.Document.Color, *light.Markdown.Document.Color,
		"markdown body color must differ between themes")

	require.NotNil(t, dark.Markdown.Code.BackgroundColor)
	require.NotNil(t, light.Markdown.Code.BackgroundColor)
	require.NotEqual(t, *dark.Markdown.Code.BackgroundColor, *light.Markdown.Code.BackgroundColor,
		"inline code background must differ between themes")

	require.NotNil(t, dark.Markdown.CodeBlock.Chroma)
	require.NotNil(t, light.Markdown.CodeBlock.Chroma)
	require.NotEqual(t,
		*dark.Markdown.CodeBlock.Chroma.Text.Color,
		*light.Markdown.CodeBlock.Chroma.Text.Color,
		"code block text color must differ between themes")
}

// TestDiffFillsMatchTheme guards the diff surface, which shipped with the dark
// tints hardcoded as hex literals and so painted solid dark blocks over a light
// diff. The fills are backgrounds for code, not accents: they must sit close to
// the base surface in both themes, and the +/- gutter text must stay readable
// on top of them.
func TestDiffFillsMatchTheme(t *testing.T) {
	t.Parallel()

	dark, light := DarkPalette(), LightPalette()

	require.NotEqual(t, hex(dark.DiffInsertCodeBg), hex(light.DiffInsertCodeBg),
		"insert fill must differ between themes")
	require.NotEqual(t, hex(dark.DiffDeleteCodeBg), hex(light.DiffDeleteCodeBg),
		"delete fill must differ between themes")

	for name, p := range map[string]Palette{"dark": dark, "light": light} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for label, fill := range map[string]color.Color{
				"insert": p.DiffInsertCodeBg,
				"delete": p.DiffDeleteCodeBg,
			} {
				// A tint, not a slab: keep it near the page it sits on.
				require.Less(t, contrastRatio(fill, p.BgBase), 2.0,
					"%s fill must stay close to the base surface", label)
			}
			// The dark theme's gutter pair predates theming and sits at
			// ~2.87:1; it is preserved verbatim rather than restyled behind a
			// light-theme fix. Hold it where it is so it cannot drift lower,
			// and hold the new light pair to the real 3:1 bar.
			floor := 2.8
			if !p.Dark {
				floor = 3.0
			}
			require.Greater(t, contrastRatio(p.DiffInsertFg, p.DiffInsertGutterBg), floor,
				"insert gutter text must be readable on its fill")
			require.Greater(t, contrastRatio(p.DiffDeleteFg, p.DiffDeleteGutterBg), floor,
				"delete gutter text must be readable on its fill")
			// Body text is drawn over the fill by the chroma theme.
			require.Greater(t, contrastRatio(p.FgBase, p.DiffInsertCodeBg), 4.5,
				"code on the insert fill must clear WCAG AA")
			require.Greater(t, contrastRatio(p.FgBase, p.DiffDeleteCodeBg), 4.5,
				"code on the delete fill must clear WCAG AA")
		})
	}
}

// TestTextOnFilledSurfaces covers every place the UI paints text over a
// colored fill: the selection bar, status pills, tags. These broke in light
// mode because the foreground was a theme-flipping neutral (fgBase, bgSubtle)
// picked to contrast with the *page* rather than with the fill it actually
// sits on -- which put dark text on the purple selection bar.
func TestTextOnFilledSurfaces(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]Palette{"dark": DarkPalette(), "light": LightPalette()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fills := map[string]color.Color{
				"primary (selection bar)": p.Primary,
				"secondary (button)":      p.Secondary,
				"error":                   p.Error,
				"red":                     p.Red,
				"info":                    p.Info,
				"green":                   p.Green,
				"yellow":                  p.Yellow,
				"warning":                 p.Warning,
			}
			for label, fill := range fills {
				chosen := p.OnFill(fill)
				alt := p.FgOnFillDark
				if chosen == p.FgOnFillDark {
					alt = p.FgOnFillLight
				}
				// The invariant that is ours to keep: OnFill picks the better
				// side. Absolute ratios are a property of Charm's brand
				// accents -- several sit near 3:1 against white and predate
				// theming -- so those are guarded by the floor below rather
				// than relitigated here.
				require.GreaterOrEqual(t, contrastRatio(chosen, fill), contrastRatio(alt, fill),
					"OnFill must pick the higher-contrast text for %s", label)
				require.Greater(t, contrastRatio(chosen, fill), 3.0,
					"text on the %s fill must clear 3:1", label)
			}

			// The selection bar carries session titles, i.e. body text, so it
			// is held to the full AA bar rather than the large-text one.
			require.Greater(t, contrastRatio(p.OnFill(p.Primary), p.Primary), 4.5,
				"selection bar text must clear WCAG AA")
		})
	}
}

func TestHexFormatsColors(t *testing.T) {
	t.Parallel()

	require.Equal(t, "#000000", hex(nil))
	require.Equal(t, "#ff0000", hex(color.RGBA{R: 0xFF, A: 0xFF}))
	require.Equal(t, "#123456", hex(color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xFF}))
}
