package model

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// applyPalette swaps the active palette in place and returns a command that
// rebuilds anything already rendered with the old colors.
//
// Every component holds the same *styles.Styles that DefaultCommon handed out,
// so overwriting the pointed-to value is enough for the tree to pick up new
// colors on its next render -- there is no per-component style to propagate.
// Two caches do need explicit invalidation:
//
//   - the glamour renderer cache keys off the *address* of the styles value,
//     which an in-place rewrite leaves unchanged;
//   - message items memoize their rendered output by content hash and width,
//     neither of which changes when only colors do.
//
// The first is cleared here; the second is handled by reloading the session so
// items rebuild through the normal path.
func (m *UI) applyPalette(p styles.Palette) tea.Cmd {
	if m == nil || m.com == nil || m.com.Styles == nil {
		return nil
	}
	if m.paletteApplied && m.com.Styles.Palette.Dark == p.Dark {
		return nil
	}

	*m.com.Styles = styles.New(p)
	m.paletteApplied = true
	common.ResetMarkdownRenderers()
	m.refreshComponentStyles()

	// Nothing rendered yet (the common case: detection resolves during Init),
	// so there is no stale output to rebuild.
	if m.session == nil || m.session.ID == "" || m.chat == nil || m.chat.Len() == 0 {
		return nil
	}
	return m.loadSession(m.session.ID)
}

// refreshComponentStyles re-pushes styles into the long-lived components built
// once in [New].
//
// Those components copy styles *by value* at construction, so rewriting
// *com.Styles leaves them rendering the previous palette -- the textarea was
// the visible case, drawing input text in the dark palette's near-white
// foreground on a light background. Dialogs are exempt: they are constructed
// each time they open and read the current styles then.
func (m *UI) refreshComponentStyles() {
	s := m.com.Styles

	m.textarea.SetStyles(s.TextArea)

	if m.completions != nil {
		m.completions.SetStyles(
			s.Completions.Normal,
			s.Completions.Focused,
			s.Completions.Match,
		)
	}
	if m.attachments != nil {
		if r := m.attachments.Renderer(); r != nil {
			r.SetStyles(
				s.Attachments.Normal,
				s.Attachments.Deleting,
				s.Attachments.Image,
				s.Attachments.Text,
			)
		}
	}
	m.todoSpinner.Style = s.Pills.TodoSpinner
}

// cycleTheme steps auto → dark → light → auto, applies the result and persists
// it globally. Returning to auto re-queries the terminal rather than keeping
// whatever palette the explicit modes left behind.
func (m *UI) cycleTheme() tea.Cmd {
	cfg := m.com.Config()
	if cfg == nil || cfg.Options == nil {
		return nil
	}

	next := styles.ThemeDark
	switch m.resolvedThemeMode() {
	case styles.ThemeDark:
		next = styles.ThemeLight
	case styles.ThemeLight:
		next = styles.ThemeAuto
	}
	cfg.Options.Theme = string(next)

	var cmds []tea.Cmd
	if next == styles.ThemeAuto {
		// Let detection decide; the reply drives applyPalette.
		cmds = append(cmds, tea.RequestBackgroundColor)
	} else if cmd := m.applyPalette(styles.PaletteFor(next, true)); cmd != nil {
		cmds = append(cmds, cmd)
	}

	cmds = append(cmds, func() tea.Msg {
		if err := m.com.Store().SetConfigField(config.ScopeGlobal, "options.theme", string(next)); err != nil {
			slog.Error("Failed to persist theme setting", "error", err)
		}
		return util.NewInfoMsg("Theme: " + string(next))
	})
	return tea.Batch(cmds...)
}

// resolvedThemeMode returns the configured theme mode.
func (m *UI) resolvedThemeMode() styles.ThemeMode {
	if m == nil || m.com == nil {
		return styles.ThemeAuto
	}
	cfg := m.com.Config()
	if cfg == nil || cfg.Options == nil {
		return styles.ThemeAuto
	}
	return styles.ParseThemeMode(cfg.Options.Theme)
}
