package model

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

type actionDialog struct {
	id     string
	action dialog.Action
}

func (d actionDialog) ID() string {
	return d.id
}

func (d actionDialog) HandleMsg(tea.Msg) dialog.Action {
	return d.action
}

func (d actionDialog) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

func TestAuthenticatedModelSelectionClosesAuthenticationDialogBeforeSwitch(t *testing.T) {
	com := testModelCommon(t)
	ui := &UI{
		com:    com,
		dialog: dialog.NewOverlay(),
		state:  uiChat,
	}

	ui.dialog.OpenDialog(actionDialog{
		id: dialog.APIKeyInputID,
		action: dialog.ActionSelectModel{
			Provider: catwalk.Provider{
				ID:   catwalk.InferenceProvider("test-provider"),
				Name: "Test Provider",
			},
			Model: config.SelectedModel{
				Provider: "test-provider",
				Model:    "test-model",
			},
			ModelType: config.SelectedModelTypeLarge,
		},
	})

	cmd := ui.handleDialogMsg(struct{}{})

	require.NotNil(t, cmd)
	require.False(t, ui.dialog.ContainsDialog(dialog.APIKeyInputID))
}

func testModelCommon(t *testing.T) *common.Common {
	t.Helper()

	baseDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(baseDir, "data-home"))
	t.Setenv("LOCALAPPDATA", filepath.Join(baseDir, "data-home"))
	t.Setenv("APPDATA", filepath.Join(baseDir, "data-home"))
	t.Setenv("USERPROFILE", baseDir)

	workingDir := filepath.Join(baseDir, "workspace")
	require.NoError(t, os.MkdirAll(workingDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "crush.json"), []byte(`{
		"options": {
			"disable_default_providers": true,
			"disable_provider_auto_update": true
		},
		"providers": {
			"test-provider": {
				"name": "Test Provider",
				"api_key": "test-key",
				"base_url": "https://example.com/v1",
				"type": "openai",
				"models": [{"id": "test-model", "name": "Test Model"}]
			}
		},
		"models": {
			"large": {"provider": "test-provider", "model": "test-model"}
		}
	}`), 0o644))

	store, err := config.Init(workingDir, filepath.Join(baseDir, "state"), false)
	require.NoError(t, err)

	dbDir := filepath.Join(baseDir, "db")
	require.NoError(t, os.MkdirAll(dbDir, 0o755))
	conn, err := db.Connect(t.Context(), dbDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	application, err := app.New(t.Context(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		application.Shutdown()
		_ = log.ResetForTesting()
	})

	application.AgentCoordinator = &mockQueueCoordinator{}
	return common.DefaultCommon(application)
}
