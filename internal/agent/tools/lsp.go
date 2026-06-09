package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

const LSPToolName = "lsp"

//go:embed lsp.md
var lspDescription []byte

// LSPParams is the unified parameter struct for the consolidated lsp tool.
type LSPParams struct {
	Op string `json:"op" description:"The LSP operation to perform. One of: diagnostics, references, declaration, definition, implementation, type_definition, hover, document_symbols, workspace_symbols, code_action, rename, format, restart."`

	// Position-based params (declaration, definition, implementation, type_definition, hover, code_action, rename).
	FilePath  string `json:"file_path,omitempty" description:"The file path containing the symbol or range to inspect."`
	Line      int    `json:"line,omitempty" description:"The 1-based line number of the symbol position."`
	Character int    `json:"character,omitempty" description:"The 1-based column number of the symbol position."`

	// Diagnostics params.
	// FilePath is reused (empty = project diagnostics).

	// References params.
	Symbol string `json:"symbol,omitempty" description:"The symbol name to search for references (references op only)."`
	Path   string `json:"path,omitempty" description:"The directory to search in for references (references op only)."`

	// Document symbols params.
	// FilePath is reused.

	// Workspace symbols params.
	Query string `json:"query,omitempty" description:"Optional symbol name query to filter workspace symbols (workspace_symbols op only)."`

	// Code action params.
	ActionKind  string `json:"action_kind,omitempty" description:"Optional code action kind filter, e.g. quickfix, refactor.extract (code_action op only)."`
	Apply       bool   `json:"apply,omitempty" description:"If true, apply a selected code action edit (code_action op only)."`
	ActionIndex int    `json:"action_index,omitempty" description:"1-based index of the action to apply when apply=true (code_action op only, defaults to 1)."`

	// Rename params.
	NewName string `json:"new_name,omitempty" description:"The new symbol name to apply (rename op only)."`

	// Format params.
	TabSize                int   `json:"tab_size,omitempty" description:"Tab width in spaces, default 4 (format op only)."`
	InsertSpaces           *bool `json:"insert_spaces,omitempty" description:"Prefer spaces over tabs, default true (format op only)."`
	TrimTrailingWhitespace *bool `json:"trim_trailing_whitespace,omitempty" description:"Trim trailing whitespace on each line (format op only)."`
	InsertFinalNewline     *bool `json:"insert_final_newline,omitempty" description:"Ensure file ends with a final newline (format op only)."`
	TrimFinalNewlines      *bool `json:"trim_final_newlines,omitempty" description:"Trim extra trailing newlines at end of file (format op only)."`

	// Restart params.
	Name string `json:"name,omitempty" description:"Optional name of a specific LSP client to restart (restart op only). Empty restarts all."`
}

// NewLSPTool creates the consolidated LSP tool that replaces all 13 individual LSP tools.
func NewLSPTool(lspManager *lsp.Manager, permissions permission.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		LSPToolName,
		string(lspDescription),
		func(ctx context.Context, params LSPParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			switch params.Op {
			case "diagnostics":
				return lspDiagnostics(ctx, lspManager, params.FilePath)
			case "references":
				return lspReferences(ctx, lspManager, params.Symbol, params.Path)
			case "declaration":
				return lspNavigate(ctx, lspManager, params, "declaration")
			case "definition":
				return lspNavigate(ctx, lspManager, params, "definition")
			case "implementation":
				return lspNavigate(ctx, lspManager, params, "implementation")
			case "type_definition":
				return lspNavigate(ctx, lspManager, params, "type definition")
			case "hover":
				return lspHover(ctx, lspManager, params)
			case "document_symbols":
				return lspDocumentSymbols(ctx, lspManager, params.FilePath)
			case "workspace_symbols":
				return lspWorkspaceSymbols(ctx, lspManager, params.Query)
			case "code_action":
				return lspCodeAction(ctx, lspManager, permissions, workingDir, params, call)
			case "rename":
				return lspRename(ctx, lspManager, permissions, workingDir, params, call)
			case "format":
				return lspFormat(ctx, lspManager, permissions, workingDir, params, call)
			case "restart":
				return lspRestart(ctx, lspManager, params.Name)
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown lsp op: %q. Valid ops: diagnostics, references, declaration, definition, implementation, type_definition, hover, document_symbols, workspace_symbols, code_action, rename, format, restart", params.Op)), nil
			}
		},
	)
}

// lspDiagnostics dispatches to the existing diagnostics logic.
func lspDiagnostics(ctx context.Context, lspManager *lsp.Manager, filePath string) (fantasy.ToolResponse, error) {
	if lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}
	notifyLSPs(ctx, lspManager, filePath)
	output := getDiagnostics(filePath, lspManager)
	return fantasy.NewTextResponse(output), nil
}

// lspReferences dispatches to the existing references logic.
func lspReferences(ctx context.Context, lspManager *lsp.Manager, symbol, path string) (fantasy.ToolResponse, error) {
	if symbol == "" {
		return fantasy.NewTextErrorResponse("symbol is required for references op"), nil
	}
	if lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}

	effectiveWorkingDir := GetWorkingDirFromContext(ctx)
	if effectiveWorkingDir == "" {
		effectiveWorkingDir = "."
	}
	wd := path
	if wd == "" {
		wd = effectiveWorkingDir
	}

	result, err := runGrepSearch(ctx, GrepParams{Pattern: symbol, LiteralText: true}, wd, 100, 0, 0)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to search for symbol: %s", err)), nil
	}
	matches := result.matches
	if len(matches) == 0 {
		return fantasy.NewTextResponse(fmt.Sprintf("Symbol '%s' not found", symbol)), nil
	}

	var allLocations []protocol.Location
	var allErrs error
	for _, match := range matches {
		locations, err := find(ctx, lspManager, symbol, match)
		if err != nil {
			if strings.Contains(err.Error(), "no identifier found") {
				continue
			}
			allErrs = errors.Join(allErrs, err)
			continue
		}
		allLocations = append(allLocations, locations...)
		if len(locations) > 0 {
			break
		}
	}

	if len(allLocations) > 0 {
		output := formatReferences(cleanupLocations(allLocations))
		return fantasy.NewTextResponse(output), nil
	}
	if allErrs != nil {
		return fantasy.NewTextErrorResponse(allErrs.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("No references found for symbol '%s'", symbol)), nil
}

// lspNavigate handles declaration, definition, implementation, type_definition.
func lspNavigate(ctx context.Context, lspManager *lsp.Manager, params LSPParams, kind string) (fantasy.ToolResponse, error) {
	client, absPath, response, ok := lspClientForPosition(ctx, lspManager, lspPositionParams{
		FilePath:  params.FilePath,
		Line:      params.Line,
		Character: params.Character,
	})
	if !ok {
		return response, nil
	}

	var (
		locations []protocol.Location
		err       error
	)
	switch kind {
	case "declaration":
		locations, err = client.FindDeclaration(ctx, absPath, params.Line, params.Character)
	case "definition":
		locations, err = client.FindDefinition(ctx, absPath, params.Line, params.Character)
	case "implementation":
		locations, err = client.FindImplementation(ctx, absPath, params.Line, params.Character)
	case "type definition":
		locations, err = client.FindTypeDefinition(ctx, absPath, params.Line, params.Character)
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	locations = cleanupLocations(locations)
	if len(locations) == 0 {
		return fantasy.NewTextResponse(fmt.Sprintf("No %s found.", kind)), nil
	}
	return fantasy.NewTextResponse(formatLocations(kind, locations)), nil
}

// lspHover handles the hover operation.
func lspHover(ctx context.Context, lspManager *lsp.Manager, params LSPParams) (fantasy.ToolResponse, error) {
	client, absPath, response, ok := lspClientForPosition(ctx, lspManager, lspPositionParams{
		FilePath:  params.FilePath,
		Line:      params.Line,
		Character: params.Character,
	})
	if !ok {
		return response, nil
	}
	hover, err := client.Hover(ctx, absPath, params.Line, params.Character)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	text := strings.TrimSpace(formatHover(hover))
	if text == "" {
		return fantasy.NewTextResponse("No hover information found."), nil
	}
	return fantasy.NewTextResponse(text), nil
}

// lspDocumentSymbols handles document symbols.
func lspDocumentSymbols(ctx context.Context, lspManager *lsp.Manager, filePath string) (fantasy.ToolResponse, error) {
	if strings.TrimSpace(filePath) == "" {
		return fantasy.NewTextErrorResponse("file_path is required for document_symbols op"), nil
	}
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}
	absPath, err := filepath.Abs(filepath.FromSlash(filePath))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get absolute path: %s", err)), nil
	}
	openInLSPs(ctx, lspManager, absPath)
	client := firstHandlingClient(lspManager, absPath)
	if client == nil {
		return fantasy.NewTextResponse(fmt.Sprintf("No LSP client handles %s", absPath)), nil
	}
	symbols, err := client.DocumentSymbols(ctx, absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(symbols) == 0 {
		return fantasy.NewTextResponse("No document symbols found."), nil
	}
	return fantasy.NewTextResponse(formatDocumentSymbols(absPath, documentSymbolEntries(symbols))), nil
}

// lspWorkspaceSymbols handles workspace symbols.
func lspWorkspaceSymbols(ctx context.Context, lspManager *lsp.Manager, query string) (fantasy.ToolResponse, error) {
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}
	var all []workspaceSymbolEntry
	for name, client := range lspManager.Clients().Seq2() {
		entries, err := client.WorkspaceSymbols(ctx, query)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("workspace symbols failed for %s: %s", name, err)), nil
		}
		all = append(all, workspaceSymbolEntries(entries)...)
	}
	if len(all) == 0 {
		return fantasy.NewTextResponse("No workspace symbols found."), nil
	}
	return fantasy.NewTextResponse(formatWorkspaceSymbols(all)), nil
}

// lspCodeAction handles code actions.
func lspCodeAction(ctx context.Context, lspManager *lsp.Manager, permissions permission.Service, workingDir string, params LSPParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	client, absPath, response, ok := lspClientForPosition(ctx, lspManager, lspPositionParams{
		FilePath:  params.FilePath,
		Line:      params.Line,
		Character: params.Character,
	})
	if !ok {
		return response, nil
	}

	only := make([]protocol.CodeActionKind, 0, 1)
	if strings.TrimSpace(params.ActionKind) != "" {
		only = append(only, protocol.CodeActionKind(strings.TrimSpace(params.ActionKind)))
	}
	actions, err := client.CodeActions(ctx, absPath, params.Line, params.Character, only)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(actions) == 0 {
		return fantasy.NewTextResponse("No code actions found."), nil
	}

	if !params.Apply {
		return fantasy.NewTextResponse(formatCodeActions(actions)), nil
	}

	selectedIndex := params.ActionIndex
	if selectedIndex <= 0 {
		selectedIndex = 1
	}
	if selectedIndex > len(actions) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("action_index %d is out of range (1-%d)", selectedIndex, len(actions))), nil
	}

	selected := actions[selectedIndex-1]
	if selected.Edit == nil {
		return fantasy.NewTextErrorResponse("selected code action does not provide a workspace edit"), nil
	}

	permissionResponse, err := requestLSPWritePermission(ctx, permissions, workingDir, call, absPath, LSPToolName, fmt.Sprintf("Apply LSP code action in %s", absPath), params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	if err := client.ApplyWorkspaceEdit(*selected.Edit); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to apply code action workspace edit: %s", err)), nil
	}
	notifyWorkspaceEditPaths(ctx, lspManager, *selected.Edit)
	return fantasy.NewTextResponse(fmt.Sprintf("Applied code action #%d: %s", selectedIndex, strings.TrimSpace(selected.Title))), nil
}

// lspRename handles symbol rename.
func lspRename(ctx context.Context, lspManager *lsp.Manager, permissions permission.Service, workingDir string, params LSPParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if strings.TrimSpace(params.NewName) == "" {
		return fantasy.NewTextErrorResponse("new_name is required for rename op"), nil
	}

	client, absPath, response, ok := lspClientForPosition(ctx, lspManager, lspPositionParams{
		FilePath:  params.FilePath,
		Line:      params.Line,
		Character: params.Character,
	})
	if !ok {
		return response, nil
	}

	workspaceEdit, err := client.Rename(ctx, absPath, params.Line, params.Character, strings.TrimSpace(params.NewName))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if workspaceEdit == nil || workspaceEditEmpty(*workspaceEdit) {
		return fantasy.NewTextResponse("Rename completed with no edits."), nil
	}

	permissionResponse, err := requestLSPWritePermission(ctx, permissions, workingDir, call, absPath, LSPToolName, fmt.Sprintf("Rename symbol in %s", absPath), params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	if err := client.ApplyWorkspaceEdit(*workspaceEdit); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to apply rename workspace edit: %s", err)), nil
	}
	notifyWorkspaceEditPaths(ctx, lspManager, *workspaceEdit)
	return fantasy.NewTextResponse(fmt.Sprintf("Renamed symbol to %s.", strings.TrimSpace(params.NewName))), nil
}

// lspFormat handles document formatting.
func lspFormat(ctx context.Context, lspManager *lsp.Manager, permissions permission.Service, workingDir string, params LSPParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	client, absPath, response, ok := lspClientForFile(ctx, lspManager, params.FilePath)
	if !ok {
		return response, nil
	}

	formatParams := LSPFormatParams{
		FilePath:               params.FilePath,
		TabSize:                params.TabSize,
		InsertSpaces:           params.InsertSpaces,
		TrimTrailingWhitespace: params.TrimTrailingWhitespace,
		InsertFinalNewline:     params.InsertFinalNewline,
		TrimFinalNewlines:      params.TrimFinalNewlines,
	}
	edits, err := client.FormatDocument(ctx, absPath, formattingOptions(formatParams))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(edits) == 0 {
		return fantasy.NewTextResponse("No formatting changes returned."), nil
	}

	workspaceEdit := protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			protocol.URIFromPath(absPath): edits,
		},
	}

	permissionResponse, err := requestLSPWritePermission(ctx, permissions, workingDir, call, absPath, LSPToolName, fmt.Sprintf("Format %s with LSP", absPath), params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	if err := client.ApplyWorkspaceEdit(workspaceEdit); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to apply formatting edits: %s", err)), nil
	}
	notifyWorkspaceEditPaths(ctx, lspManager, workspaceEdit)
	return fantasy.NewTextResponse(fmt.Sprintf("Applied %d formatting edit(s).", len(edits))), nil
}

// lspRestart handles LSP client restart.
func lspRestart(ctx context.Context, lspManager *lsp.Manager, name string) (fantasy.ToolResponse, error) {
	if lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available to restart"), nil
	}

	clientsToRestart := make(map[string]*lsp.Client)
	if name == "" {
		maps.Insert(clientsToRestart, lspManager.Clients().Seq2())
	} else {
		client, exists := lspManager.Clients().Get(name)
		if !exists {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("LSP client '%s' not found", name)), nil
		}
		clientsToRestart[name] = client
	}

	var restarted []string
	var failed []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for clientName, client := range clientsToRestart {
		clientName, client := clientName, client
		wg.Go(func() {
			if err := client.Restart(ctx); err != nil {
				slog.Error("Failed to restart LSP client", "name", clientName, "error", err)
				mu.Lock()
				failed = append(failed, clientName)
				mu.Unlock()
				return
			}
			mu.Lock()
			restarted = append(restarted, clientName)
			mu.Unlock()
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}

	var output string
	if len(restarted) > 0 {
		output = fmt.Sprintf("Successfully restarted %d LSP client(s): %s\n", len(restarted), strings.Join(restarted, ", "))
	}
	if len(failed) > 0 {
		output += fmt.Sprintf("Failed to restart %d LSP client(s): %s\n", len(failed), strings.Join(failed, ", "))
		return fantasy.NewTextErrorResponse(output), nil
	}
	return fantasy.NewTextResponse(output), nil
}
