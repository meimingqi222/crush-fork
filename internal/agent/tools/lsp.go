package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

const LSPToolName = "lsp"

// lspUnknownOpMessage is the recovery hint returned for an unrecognized op.
// It must list exactly the ops NewLSPTool dispatches; TestLSPUnknownOpErrorListsRealOps
// pins that, along with the LSPParams.Op schema description.
const lspUnknownOpMessage = "Valid ops: diagnostics, references, definition, document_symbols, " +
	"workspace_symbols, code_action, rename, replace_symbol, call_hierarchy, restart"

//go:embed lsp.md
var lspDescription []byte

// LSPParams is the unified parameter struct for the consolidated lsp tool.
type LSPParams struct {
	Op string `json:"op" description:"The LSP operation to perform. One of: diagnostics, references, definition, document_symbols, workspace_symbols, code_action, rename, replace_symbol, call_hierarchy, restart."`

	// Position-based params (code_action, rename, and call_hierarchy when no
	// symbol name is given).
	FilePath  string `json:"file_path,omitempty" description:"The file path containing the symbol or range to inspect."`
	Line      int    `json:"line,omitempty" description:"The 1-based line number of the symbol position."`
	Character int    `json:"character,omitempty" description:"The 1-based column number of the symbol position."`

	// Diagnostics params.
	// FilePath is reused (empty = project diagnostics).

	// Name-based params, shared by references, definition, replace_symbol and
	// call_hierarchy. Prefer these over Line/Character: they are
	// language-aware and skip matches in comments, strings and partial
	// identifiers.
	Symbol string `json:"symbol,omitempty" description:"The symbol name to look up (references, definition, replace_symbol, call_hierarchy)."`
	Path   string `json:"path,omitempty" description:"The directory to search in when resolving a symbol by name. Defaults to the working directory."`

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

	// Replace symbol params.
	Replacement string `json:"replacement,omitempty" description:"The replacement text. Required for 'replace'/'add_before'/'add_after' actions (replace_symbol op only)."`
	Action      string `json:"action,omitempty" description:"The action to perform for replace_symbol: replace (default), add_before, add_after, or delete."`

	// Call hierarchy params.
	// Symbol is preferred; FilePath / Line / Character are reused when
	// resolving by position instead.

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
			case "definition":
				return lspDefinition(ctx, lspManager, params)
			case "document_symbols":
				return lspDocumentSymbols(ctx, lspManager, params.FilePath)
			case "workspace_symbols":
				return lspWorkspaceSymbols(ctx, lspManager, params.Query)
			case "code_action":
				return lspCodeAction(ctx, lspManager, permissions, workingDir, params, call)
			case "rename":
				return lspRename(ctx, lspManager, permissions, workingDir, params, call)
			case "replace_symbol":
				return lspReplaceSymbol(ctx, lspManager, permissions, workingDir, params, call)
			case "call_hierarchy":
				return lspCallHierarchy(ctx, lspManager, params)
			case "restart":
				return lspRestart(ctx, lspManager, params.Name)
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown lsp op: %q. %s", params.Op, lspUnknownOpMessage)), nil
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

// lspDefinition resolves a symbol by name and returns its definition
// locations. Name-based resolution is more robust than requiring the model to
// supply exact 1-based coordinates.
func lspDefinition(ctx context.Context, lspManager *lsp.Manager, params LSPParams) (fantasy.ToolResponse, error) {
	if strings.TrimSpace(params.Symbol) == "" {
		return fantasy.NewTextErrorResponse("symbol is required for definition op"), nil
	}
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}

	wd := params.Path
	if wd == "" {
		wd = GetWorkingDirFromContext(ctx)
	}
	if wd == "" {
		wd = "."
	}
	resolved, err := resolveSymbol(ctx, lspManager, params.Symbol, wd)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Symbol '%s' not found", params.Symbol)), nil
	}

	locations, err := resolved.client.FindDefinition(ctx, resolved.path, resolved.line, resolved.char)
	if err != nil {
		if isNoIdentifierError(err) {
			return fantasy.NewTextResponse(fmt.Sprintf("Symbol '%s' not found as an identifier", params.Symbol)), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	locations = cleanupLocations(locations)
	if len(locations) == 0 {
		return fantasy.NewTextResponse(fmt.Sprintf("No definition found for '%s'.", params.Symbol)), nil
	}
	return fantasy.NewTextResponse(formatLocations("definition", locations)), nil
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

// lspReplaceSymbol replaces, inserts before/after, or deletes a symbol by
// name using the LSP document-symbol tree, so the edit is anchored to the
// symbol's semantic range rather than a fragile text match.
func lspReplaceSymbol(ctx context.Context, lspManager *lsp.Manager, permissions permission.Service, workingDir string, params LSPParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if strings.TrimSpace(params.FilePath) == "" {
		return fantasy.NewTextErrorResponse("file_path is required for replace_symbol op"), nil
	}
	symbol := strings.TrimSpace(params.Symbol)
	if symbol == "" {
		return fantasy.NewTextErrorResponse("symbol is required for replace_symbol op"), nil
	}
	action := strings.TrimSpace(params.Action)
	if action == "" {
		action = "replace"
	}
	switch action {
	case "replace", "add_before", "add_after", "delete":
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid action %q: must be replace, add_before, add_after, or delete", action)), nil
	}
	if (action == "replace" || action == "add_before" || action == "add_after") && params.Replacement == "" {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("replacement is required for action %q", action)), nil
	}

	absPath, err := filepath.Abs(filepath.FromSlash(params.FilePath))
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get absolute path: %s", err)), nil
	}
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}
	openInLSPs(ctx, lspManager, absPath)
	client := firstHandlingClient(lspManager, absPath)
	if client == nil {
		return fantasy.NewTextResponse(fmt.Sprintf("No LSP client handles %s", absPath)), nil
	}

	symbols, err := client.DocumentSymbols(ctx, absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to get document symbols: %s", err)), nil
	}
	target := findSymbolByName(symbols, symbol)
	if target == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("symbol '%s' not found in %s", symbol, absPath)), nil
	}
	rng := target.GetRange()

	content, err := os.ReadFile(absPath)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
	}
	lines := strings.Split(string(content), "\n")
	startLine := int(rng.Start.Line)
	endLine := int(rng.End.Line)
	if startLine >= len(lines) || endLine >= len(lines) {
		return fantasy.NewTextErrorResponse("symbol range exceeds file length"), nil
	}

	var newLines []string
	switch action {
	case "replace":
		newLines = make([]string, 0, len(lines))
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
		newLines = append(newLines, lines[endLine+1:]...)
	case "add_before":
		newLines = make([]string, 0, len(lines)+strings.Count(params.Replacement, "\n")+1)
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
		newLines = append(newLines, lines[startLine:]...)
	case "add_after":
		newLines = make([]string, 0, len(lines)+strings.Count(params.Replacement, "\n")+1)
		newLines = append(newLines, lines[:endLine+1]...)
		newLines = append(newLines, strings.Split(params.Replacement, "\n")...)
		newLines = append(newLines, lines[endLine+1:]...)
	case "delete":
		newLines = make([]string, 0, len(lines))
		newLines = append(newLines, lines[:startLine]...)
		newLines = append(newLines, lines[endLine+1:]...)
	}
	newContent := strings.Join(newLines, "\n")

	permissionResponse, err := requestLSPWritePermission(ctx, permissions, workingDir, call, absPath, LSPToolName, fmt.Sprintf("%s symbol '%s' in %s", action, symbol, absPath), params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if permissionResponse != nil {
		return *permissionResponse, nil
	}

	if err := os.WriteFile(absPath, []byte(newContent), 0o644); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
	}
	notifyLSPs(ctx, lspManager, absPath)

	var summary string
	switch action {
	case "replace":
		summary = fmt.Sprintf("Replaced symbol '%s' in %s (lines %d-%d)", symbol, absPath, startLine+1, endLine+1)
	case "add_before":
		summary = fmt.Sprintf("Inserted before symbol '%s' in %s (before line %d)", symbol, absPath, startLine+1)
	case "add_after":
		summary = fmt.Sprintf("Inserted after symbol '%s' in %s (after line %d)", symbol, absPath, endLine+1)
	case "delete":
		summary = fmt.Sprintf("Deleted symbol '%s' from %s (lines %d-%d)", symbol, absPath, startLine+1, endLine+1)
	}
	resp := fantasy.NewTextResponse(summary + "\n" + getDiagnostics(absPath, lspManager))
	return resp, nil
}

// findSymbolByName searches for a symbol by name in the document symbol tree.
func findSymbolByName(symbols []protocol.DocumentSymbolResult, name string) protocol.DocumentSymbolResult {
	for _, sym := range symbols {
		if sym.GetName() == name {
			return sym
		}
		if ds, ok := sym.(*protocol.DocumentSymbol); ok && len(ds.Children) > 0 {
			children := make([]protocol.DocumentSymbolResult, len(ds.Children))
			for i := range ds.Children {
				children[i] = &ds.Children[i]
			}
			if found := findSymbolByName(children, name); found != nil {
				return found
			}
		}
	}
	return nil
}

// lspCallHierarchy resolves a symbol to a position and returns its incoming
// (callers) and outgoing (callees) call hierarchy.
func lspCallHierarchy(ctx context.Context, lspManager *lsp.Manager, params LSPParams) (fantasy.ToolResponse, error) {
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return fantasy.NewTextErrorResponse("no LSP clients available"), nil
	}

	var (
		client     *lsp.Client
		absPath    string
		line, char int
	)
	if strings.TrimSpace(params.Symbol) != "" {
		wd := GetWorkingDirFromContext(ctx)
		if wd == "" {
			wd = "."
		}
		resolved, err := resolveSymbol(ctx, lspManager, params.Symbol, wd)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		client = resolved.client
		absPath = resolved.path
		line = resolved.line
		char = resolved.char
	} else {
		c, p, resp, ok := lspClientForPosition(ctx, lspManager, lspPositionParams{
			FilePath:  params.FilePath,
			Line:      params.Line,
			Character: params.Character,
		})
		if !ok {
			return resp, nil
		}
		client = c
		absPath = p
		line = params.Line
		char = params.Character
	}

	items, err := client.PrepareCallHierarchy(ctx, absPath, line, char)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if len(items) == 0 {
		return fantasy.NewTextResponse("No call hierarchy found for this symbol."), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d call hierarchy root(s):\n", len(items))
	for _, item := range items {
		fmt.Fprintf(&sb, "- %s (%s:%d)\n", item.Name, item.URI, item.Range.Start.Line+1)
		incoming, err := client.IncomingCalls(ctx, item)
		if err == nil && len(incoming) > 0 {
			fmt.Fprintf(&sb, "  Callers (%d):\n", len(incoming))
			for _, call := range incoming {
				fmt.Fprintf(&sb, "    - %s (%s:%d)\n", call.From.Name, call.From.URI, call.From.Range.Start.Line+1)
			}
		}
		outgoing, err := client.OutgoingCalls(ctx, item)
		if err == nil && len(outgoing) > 0 {
			fmt.Fprintf(&sb, "  Callees (%d):\n", len(outgoing))
			for _, call := range outgoing {
				fmt.Fprintf(&sb, "    - %s (%s:%d)\n", call.To.Name, call.To.URI, call.To.Range.Start.Line+1)
			}
		}
	}
	return fantasy.NewTextResponse(sb.String()), nil
}
