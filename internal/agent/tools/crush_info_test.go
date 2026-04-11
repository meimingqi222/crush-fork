package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/stretchr/testify/require"
)

func loadCrushInfoTestStore(t *testing.T) *config.ConfigStore {
	t.Helper()

	workingDir := t.TempDir()
	globalConfigRoot := filepath.Join(t.TempDir(), "crush-info-global")
	globalDataRoot := filepath.Join(t.TempDir(), "crush-info-data")
	require.NoError(t, os.MkdirAll(globalConfigRoot, 0o755))
	require.NoError(t, os.MkdirAll(globalDataRoot, 0o755))

	t.Setenv("CRUSH_GLOBAL_CONFIG", globalConfigRoot)
	t.Setenv("CRUSH_GLOBAL_DATA", globalDataRoot)
	t.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")

	payload, err := json.Marshal(map[string]any{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "crush.json"), payload, 0o600))

	store, err := config.Load(workingDir, "", false)
	require.NoError(t, err)
	return store
}

func writeSkillFile(t *testing.T, rootDir, name, description string) {
	t.Helper()

	skillDir := filepath.Join(rootDir, name)
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nUse " + name + ".\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600))
}

func TestCrushInfoToolNameConstant(t *testing.T) {
	require.Equal(t, "crush_info", CrushInfoToolName)
}

func TestCrushInfo_HighValueSections(t *testing.T) {
	store := loadCrushInfoTestStore(t)
	cfg := store.Config()

	skillsDir := t.TempDir()
	writeSkillFile(t, skillsDir, "zeta-skill", "zeta")
	writeSkillFile(t, skillsDir, "alpha-skill", "alpha")

	cfg.Models = map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge:          {Model: "model-large", Provider: "alpha"},
		config.SelectedModelTypeSmall:          {Model: "model-small", Provider: "alpha"},
		config.SelectedModelTypeBackground:     {Model: "model-bg", Provider: "alpha"},
		config.SelectedModelTypeAutoClassifier: {Model: "model-auto", Provider: "alpha"},
	}

	cfg.Providers = csync.NewMap[string, config.ProviderConfig]()
	cfg.Providers.Set("zeta", config.ProviderConfig{Models: []catwalk.Model{{ID: "zeta-1"}}})
	cfg.Providers.Set("alpha", config.ProviderConfig{Models: []catwalk.Model{{ID: "alpha-1"}, {ID: "alpha-2"}}})

	cfg.LSP = config.LSPs{
		"gopls":         {Disabled: false},
		"rust-analyzer": {Disabled: true},
	}
	cfg.MCP = config.MCPs{
		"filesystem": {Type: config.MCPStdio, Command: "fs"},
		"docker":     {Type: config.MCPStdio, Command: "docker", Disabled: true},
	}

	cfg.Options.SkillsPaths = []string{skillsDir}
	cfg.Options.DisabledTools = []string{"sourcegraph", "agentic_fetch"}
	cfg.Options.DisableAutoSummarize = true
	cfg.Options.Attribution = &config.Attribution{TrailerStyle: config.TrailerStyleAssistedBy, GeneratedWith: true}

	cfg.Permissions = &config.Permissions{AllowedTools: []string{"edit:write", "bash"}, SkipRequests: true}

	lspManager := lsp.NewManager(store)
	readyClient := &lsp.Client{}
	readyClient.SetServerState(lsp.StateReady)
	lspManager.Clients().Set("gopls", readyClient)

	output := buildCrushInfo(store, lspManager)

	require.Contains(t, output, "[model]")
	require.Contains(t, output, "[providers]")
	require.Contains(t, output, "[lsp]")
	require.Contains(t, output, "[lsp_configured]")
	require.Contains(t, output, "[mcp_configured]")
	require.Contains(t, output, "[skills]")
	require.Contains(t, output, "[permissions]")
	require.Contains(t, output, "[tools]")
	require.Contains(t, output, "[options]")
	require.Contains(t, output, "[attribution]")

	require.Contains(t, output, "allowed_tools = bash, edit:write")
	require.Contains(t, output, "disabled = agentic_fetch, sourcegraph")
	require.Contains(t, output, "mode = yolo")
	require.Contains(t, output, "trailer_style = assisted-by")
	require.Contains(t, output, "generated_with = true")
	require.Contains(t, output, "alpha-skill = available")
	require.Contains(t, output, "zeta-skill = available")

	alphaProviderIndex := strings.Index(output, "alpha = enabled")
	zetaProviderIndex := strings.Index(output, "zeta = enabled")
	require.GreaterOrEqual(t, alphaProviderIndex, 0)
	require.GreaterOrEqual(t, zetaProviderIndex, 0)
	require.Less(t, alphaProviderIndex, zetaProviderIndex)
}

func TestCrushInfo_MCPRuntimeFormatting(t *testing.T) {
	store := loadCrushInfoTestStore(t)
	states := map[string]mcp.ClientInfo{
		"github": {
			Name:        "github",
			State:       mcp.StateConnected,
			Counts:      mcp.Counts{Tools: 3, Resources: 1},
			ConnectedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		"filesystem": {
			Name:  "filesystem",
			State: mcp.StateError,
			Error: errors.New("connection refused"),
		},
	}

	var builder strings.Builder
	writeMCP(&builder, states, store)
	output := builder.String()

	require.Contains(t, output, "[mcp]")
	require.Contains(t, output, "filesystem = error: connection refused")
	require.Contains(t, output, "github = connected (3 tools, 1 resources) since 03:04:05")
}

func TestCrushInfo_NoSecretsLeak(t *testing.T) {
	store := loadCrushInfoTestStore(t)
	cfg := store.Config()

	cfg.Providers = csync.NewMap[string, config.ProviderConfig]()
	cfg.Providers.Set("openai", config.ProviderConfig{
		APIKey: "sk-super-secret-key-12345",
		Models: []catwalk.Model{{ID: "gpt-4o"}},
	})

	output := buildCrushInfo(store, nil)
	require.NotContains(t, output, "sk-super-secret-key-12345")
	require.NotContains(t, output, "secret-key")
	require.Contains(t, output, "openai = enabled (1 models)")
}

func TestCrushInfoToolRun(t *testing.T) {
	store := loadCrushInfoTestStore(t)
	tool := NewCrushInfoTool(store, nil)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Name: CrushInfoToolName, Input: "{}"})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "[options]")
}
