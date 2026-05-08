package dialog

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestPermissionsHasDiffViewForEdit(t *testing.T) {
	t.Parallel()

	d := &Permissions{
		permission: permission.PermissionRequest{ToolName: tools.EditToolName},
	}

	require.True(t, d.hasDiffView())
}

func TestPermissionsRenderHeaderIncludesEditFile(t *testing.T) {
	com := testCommon(t, `{"options":{"disable_provider_auto_update":true},"tools":{}}`)
	d := NewPermissions(com, permission.PermissionRequest{
		ToolName: tools.EditToolName,
		Path:     "/workspace",
		Params: tools.EditPermissionsParams{
			FilePath: "/workspace/internal/agent/tools/hashline.go",
		},
	})

	header := d.renderHeader(120)
	require.Contains(t, header, "hashline.go")
}
