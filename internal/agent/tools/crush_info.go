package tools

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory/engine"
	"github.com/charmbracelet/crush/internal/skills"
)

const CrushInfoToolName = "crush_info"

//go:embed crush_info.md
var crushInfoDescription []byte

type CrushInfoParams struct{}

func NewCrushInfoTool(cfg *config.ConfigStore, lspManager *lsp.Manager, memEng *engine.Engine) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		CrushInfoToolName,
		string(crushInfoDescription),
		func(ctx context.Context, _ CrushInfoParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(buildCrushInfo(ctx, cfg, lspManager, memEng)), nil
		},
	)
}

func buildCrushInfo(ctx context.Context, cfg *config.ConfigStore, lspManager *lsp.Manager, memEng *engine.Engine) string {
	if cfg == nil {
		return ""
	}

	var b strings.Builder
	writePaths(&b, cfg)
	writeContextFiles(&b, cfg)
	writeModels(&b, cfg)
	writeProviders(&b, cfg)
	writeLSP(&b, lspManager, cfg)
	writeMCP(&b, mcp.GetStates(), cfg)
	writeMemory(&b, ctx, memEng, cfg)
	writeSkills(&b, cfg)
	writePermissions(&b, cfg)
	writeDisabledTools(&b, cfg)
	writeOptions(&b, cfg)
	writeAttribution(&b, cfg)
	return b.String()
}

func writePaths(b *strings.Builder, cfg *config.ConfigStore) {
	b.WriteString("[paths]\n")
	fmt.Fprintf(b, "working_dir = %s\n", cfg.WorkingDir())
	if pdd := cfg.ProjectDataDir(); pdd != "" {
		fmt.Fprintf(b, "project_data_dir = %s\n", pdd)
		fmt.Fprintf(b, "database = %s\n", filepath.Join(pdd, "crush.db"))
	}
	fmt.Fprintf(b, "global_config = %s\n", config.GlobalConfig())
	fmt.Fprintf(b, "global_data = %s\n", config.GlobalConfigData())
	b.WriteString("\n")
}

func writeContextFiles(b *strings.Builder, cfg *config.ConfigStore) {
	workingDir := cfg.WorkingDir()
	type contextEntry struct {
		path   string
		global bool
	}
	var active []contextEntry

	for _, name := range config.DefaultContextPaths() {
		p := filepath.Join(workingDir, name)
		if _, err := os.Stat(p); err == nil {
			active = append(active, contextEntry{path: name})
		}
	}

	if cfg.Config().Options != nil {
		for _, cp := range cfg.Config().Options.ContextPaths {
			if cp == "" {
				continue
			}
			if !filepath.IsAbs(cp) {
				cp = filepath.Join(workingDir, cp)
			}
			if _, err := os.Stat(cp); err == nil {
				active = append(active, contextEntry{path: cp})
			}
		}
	}

	globalAgents := config.GlobalAgentsMD()
	if _, err := os.Stat(globalAgents); err == nil {
		active = append(active, contextEntry{path: globalAgents, global: true})
	}

	if len(active) == 0 {
		return
	}

	b.WriteString("[context_files]\n")
	for _, entry := range active {
		if entry.global {
			fmt.Fprintf(b, "%s = loaded (global)\n", entry.path)
		} else {
			fmt.Fprintf(b, "%s = loaded\n", entry.path)
		}
	}
	b.WriteString("\n")
}

func writeMemory(b *strings.Builder, ctx context.Context, eng *engine.Engine, cfg *config.ConfigStore) {
	if eng == nil {
		return
	}

	b.WriteString("[memory]\n")
	fmt.Fprintf(b, "backend = %s\n", eng.Backend())
	if !eng.Enabled() {
		b.WriteString("enabled = false\n")
		b.WriteString("\n")
		return
	}
	b.WriteString("enabled = true\n")

	if eng.IsDegraded() {
		fmt.Fprintf(b, "degraded = %s\n", eng.DegradedReason())
	}

	if store := eng.EventStore(); store != nil {
		if wm, err := store.GetMaxWatermark(ctx); err == nil {
			fmt.Fprintf(b, "max_watermark = %d\n", wm)
		}
	}

	status, err := eng.Status(ctx)
	if err == nil && status != nil {
		if status.ExtractionStatus.LastRunAt != nil {
			fmt.Fprintf(b, "extraction = %s (last: %s)\n",
				status.ExtractionStatus.State,
				status.ExtractionStatus.LastRunAt.Format("15:04:05"),
			)
		} else {
			fmt.Fprintf(b, "extraction = %s\n", status.ExtractionStatus.State)
		}
		if status.ConsolidationStatus.LastRunAt != nil {
			fmt.Fprintf(b, "consolidation = %s (last: %s, watermark: %d)\n",
				status.ConsolidationStatus.State,
				status.ConsolidationStatus.LastRunAt.Format("15:04:05"),
				status.ConsolidationStatus.LastWatermark,
			)
		} else {
			fmt.Fprintf(b, "consolidation = %s\n", status.ConsolidationStatus.State)
		}
		for _, view := range status.MaterializationViews {
			if view.LastUpdatedAt != nil {
				fmt.Fprintf(b, "view %s = watermark=%d, updated=%s\n",
					view.ViewName, view.Watermark, view.LastUpdatedAt.Format("2006-01-02 15:04:05"),
				)
			} else {
				fmt.Fprintf(b, "view %s = watermark=%d\n", view.ViewName, view.Watermark)
			}
		}
	}

	if cfg != nil && cfg.Config().Options != nil {
		memoryDir := filepath.Join(cfg.Config().Options.DataDirectory, "memory")
		summaryPath := filepath.Join(memoryDir, "memory_summary.md")
		if _, err := os.Stat(summaryPath); err == nil {
			fmt.Fprintf(b, "summary_file = %s (exists)\n", summaryPath)
		} else {
			fmt.Fprintf(b, "summary_file = %s (missing)\n", summaryPath)
		}
	}

	b.WriteString("\n")
}

func writeModels(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if len(c.Models) == 0 {
		return
	}

	b.WriteString("[model]\n")
	for _, typ := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall, config.SelectedModelTypeBackground, config.SelectedModelTypeAutoClassifier} {
		model, ok := c.Models[typ]
		if !ok || model.Model == "" || model.Provider == "" {
			continue
		}
		fmt.Fprintf(b, "%s = %s (%s)\n", typ, model.Model, model.Provider)
	}
	b.WriteString("\n")
}

func writeProviders(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	type providerEntry struct {
		name  string
		count int
	}

	var providers []providerEntry
	for name, provider := range c.Providers.Seq2() {
		if provider.Disable {
			continue
		}
		providers = append(providers, providerEntry{name: name, count: len(provider.Models)})
	}
	if len(providers) == 0 {
		return
	}

	slices.SortFunc(providers, func(a, b providerEntry) int {
		return strings.Compare(a.name, b.name)
	})

	b.WriteString("[providers]\n")
	for _, provider := range providers {
		fmt.Fprintf(b, "%s = enabled (%d models)\n", provider.name, provider.count)
	}
	b.WriteString("\n")
}

func writeLSP(b *strings.Builder, lspManager *lsp.Manager, cfg *config.ConfigStore) {
	if lspManager != nil && lspManager.Clients().Len() > 0 {
		type runtimeEntry struct {
			name  string
			state lsp.ServerState
		}

		var runtime []runtimeEntry
		for name, client := range lspManager.Clients().Seq2() {
			runtime = append(runtime, runtimeEntry{name: name, state: client.GetServerState()})
		}
		slices.SortFunc(runtime, func(a, b runtimeEntry) int {
			return strings.Compare(a.name, b.name)
		})

		b.WriteString("[lsp]\n")
		for _, entry := range runtime {
			fmt.Fprintf(b, "%s = %s\n", entry.name, lspStateString(entry.state))
		}
		b.WriteString("\n")
	}

	configured := cfg.Config().LSP
	if len(configured) == 0 {
		return
	}

	runtimeNames := make(map[string]struct{})
	if lspManager != nil {
		for name := range lspManager.Clients().Seq2() {
			runtimeNames[name] = struct{}{}
		}
	}

	type configuredEntry struct {
		name   string
		status string
	}

	entries := make([]configuredEntry, 0, len(configured))
	for name, lspCfg := range configured {
		if _, ok := runtimeNames[name]; ok {
			continue
		}
		status := "not_started"
		if lspCfg.Disabled {
			status = "disabled"
		}
		entries = append(entries, configuredEntry{name: name, status: status})
	}
	if len(entries) == 0 {
		return
	}

	slices.SortFunc(entries, func(a, b configuredEntry) int {
		return strings.Compare(a.name, b.name)
	})

	b.WriteString("[lsp_configured]\n")
	for _, entry := range entries {
		fmt.Fprintf(b, "%s = %s\n", entry.name, entry.status)
	}
	b.WriteString("\n")
}

func writeMCP(b *strings.Builder, states map[string]mcp.ClientInfo, cfg *config.ConfigStore) {
	if len(states) > 0 {
		type runtimeEntry struct {
			name        string
			state       mcp.State
			err         error
			tools       int
			resources   int
			connectedAt string
		}

		entries := make([]runtimeEntry, 0, len(states))
		for name, info := range states {
			entry := runtimeEntry{name: name, state: info.State, err: info.Error}
			if info.State == mcp.StateConnected {
				entry.tools = info.Counts.Tools
				entry.resources = info.Counts.Resources
				if !info.ConnectedAt.IsZero() {
					entry.connectedAt = info.ConnectedAt.Format("15:04:05")
				}
			}
			entries = append(entries, entry)
		}

		slices.SortFunc(entries, func(a, b runtimeEntry) int {
			return strings.Compare(a.name, b.name)
		})

		b.WriteString("[mcp]\n")
		for _, entry := range entries {
			switch entry.state {
			case mcp.StateConnected:
				if entry.connectedAt == "" {
					fmt.Fprintf(b, "%s = connected (%d tools, %d resources)\n", entry.name, entry.tools, entry.resources)
				} else {
					fmt.Fprintf(b, "%s = connected (%d tools, %d resources) since %s\n", entry.name, entry.tools, entry.resources, entry.connectedAt)
				}
			case mcp.StateError:
				if entry.err == nil {
					fmt.Fprintf(b, "%s = error\n", entry.name)
				} else {
					fmt.Fprintf(b, "%s = error: %s\n", entry.name, entry.err.Error())
				}
			default:
				fmt.Fprintf(b, "%s = %s\n", entry.name, entry.state)
			}
		}
		b.WriteString("\n")
	}

	configured := cfg.Config().MCP
	if len(configured) == 0 {
		return
	}

	runtimeNames := make(map[string]struct{}, len(states))
	for name := range states {
		runtimeNames[name] = struct{}{}
	}

	type configuredEntry struct {
		name   string
		status string
	}

	entries := make([]configuredEntry, 0, len(configured))
	for name, mcpCfg := range configured {
		if _, ok := runtimeNames[name]; ok {
			continue
		}
		status := "not_started"
		if mcpCfg.Disabled {
			status = "disabled"
		}
		entries = append(entries, configuredEntry{name: name, status: status})
	}
	if len(entries) == 0 {
		return
	}

	slices.SortFunc(entries, func(a, b configuredEntry) int {
		return strings.Compare(a.name, b.name)
	})

	b.WriteString("[mcp_configured]\n")
	for _, entry := range entries {
		fmt.Fprintf(b, "%s = %s\n", entry.name, entry.status)
	}
	b.WriteString("\n")
}

func writeSkills(b *strings.Builder, cfg *config.ConfigStore) {
	c := cfg.Config()
	if c.Options == nil || len(c.Options.SkillsPaths) == 0 {
		return
	}

	discovered := skills.Discover(c.Options.SkillsPaths)
	if len(discovered) == 0 {
		return
	}

	names := make([]string, 0, len(discovered))
	seen := make(map[string]struct{}, len(discovered))
	for _, skill := range discovered {
		if skill == nil {
			continue
		}
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}

	slices.Sort(names)
	b.WriteString("[skills]\n")
	for _, name := range names {
		fmt.Fprintf(b, "%s = available\n", name)
	}
	b.WriteString("\n")
}

func writePermissions(b *strings.Builder, cfg *config.ConfigStore) {
	permissions := cfg.Config().Permissions
	if permissions == nil {
		return
	}

	if !permissions.SkipRequests && len(permissions.AllowedTools) == 0 {
		return
	}

	b.WriteString("[permissions]\n")
	if permissions.SkipRequests {
		b.WriteString("mode = yolo\n")
	}
	if len(permissions.AllowedTools) > 0 {
		allowed := slices.Clone(permissions.AllowedTools)
		slices.Sort(allowed)
		fmt.Fprintf(b, "allowed_tools = %s\n", strings.Join(allowed, ", "))
	}
	b.WriteString("\n")
}

func writeDisabledTools(b *strings.Builder, cfg *config.ConfigStore) {
	options := cfg.Config().Options
	if options == nil || len(options.DisabledTools) == 0 {
		return
	}

	disabled := slices.Clone(options.DisabledTools)
	slices.Sort(disabled)

	b.WriteString("[tools]\n")
	fmt.Fprintf(b, "disabled = %s\n", strings.Join(disabled, ", "))
	b.WriteString("\n")
}

func writeOptions(b *strings.Builder, cfg *config.ConfigStore) {
	options := cfg.Config().Options
	if options == nil {
		return
	}

	type optionLine struct {
		key   string
		value string
	}

	autoLSP := options.AutoLSP == nil || *options.AutoLSP
	autoSummarize := !options.DisableAutoSummarize

	lines := []optionLine{
		{key: "auto_lsp", value: fmt.Sprintf("%v", autoLSP)},
		{key: "auto_summarize", value: fmt.Sprintf("%v", autoSummarize)},
		{key: "data_directory", value: options.DataDirectory},
		{key: "debug", value: fmt.Sprintf("%v", options.Debug)},
	}

	slices.SortFunc(lines, func(a, b optionLine) int {
		return strings.Compare(a.key, b.key)
	})

	b.WriteString("[options]\n")
	for _, line := range lines {
		fmt.Fprintf(b, "%s = %s\n", line.key, line.value)
	}
	b.WriteString("\n")
}

func writeAttribution(b *strings.Builder, cfg *config.ConfigStore) {
	options := cfg.Config().Options
	if options == nil || options.Attribution == nil {
		return
	}

	trailerStyle := options.Attribution.TrailerStyle
	if trailerStyle == "" {
		trailerStyle = config.TrailerStyleAssistedBy
	}

	b.WriteString("[attribution]\n")
	fmt.Fprintf(b, "trailer_style = %s\n", trailerStyle)
	fmt.Fprintf(b, "generated_with = %v\n", options.Attribution.GeneratedWith)
	b.WriteString("\n")
}

func lspStateString(state lsp.ServerState) string {
	switch state {
	case lsp.StateUnstarted:
		return "unstarted"
	case lsp.StateStarting:
		return "starting"
	case lsp.StateReady:
		return "ready"
	case lsp.StateError:
		return "error"
	case lsp.StateStopped:
		return "stopped"
	case lsp.StateDisabled:
		return "disabled"
	case lsp.StateIndexing:
		return "indexing"
	default:
		return "unknown"
	}
}
