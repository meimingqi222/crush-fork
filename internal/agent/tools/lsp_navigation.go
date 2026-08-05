package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

type lspPositionParams struct {
	FilePath  string
	Line      int
	Character int
}

const (
	LSPDeclarationToolName      = "lsp_declaration"
	LSPDefinitionToolName       = "lsp_definition"
	LSPImplementationToolName   = "lsp_implementation"
	LSPTypeDefinitionToolName   = "lsp_type_definition"
	LSPHoverToolName            = "lsp_hover"
	LSPDocumentSymbolsToolName  = "lsp_document_symbols"
	LSPWorkspaceSymbolsToolName = "lsp_workspace_symbols"
)

func lspClientForPosition(ctx context.Context, lspManager *lsp.Manager, params lspPositionParams) (*lsp.Client, string, fantasy.ToolResponse, bool) {
	if strings.TrimSpace(params.FilePath) == "" {
		return nil, "", fantasy.NewTextErrorResponse("file_path is required"), false
	}
	if params.Line <= 0 {
		return nil, "", fantasy.NewTextErrorResponse("line must be >= 1"), false
	}
	if params.Character <= 0 {
		return nil, "", fantasy.NewTextErrorResponse("character must be >= 1"), false
	}
	if lspManager == nil || lspManager.Clients().Len() == 0 {
		return nil, "", fantasy.NewTextErrorResponse("no LSP clients available"), false
	}

	absPath := ResolveToolPath(ctx, "", params.FilePath).AbsolutePath
	openInLSPs(ctx, lspManager, absPath)
	client := firstHandlingClient(lspManager, absPath)
	if client == nil {
		return nil, absPath, fantasy.NewTextResponse(fmt.Sprintf("No LSP client handles %s", absPath)), false
	}
	return client, absPath, fantasy.ToolResponse{}, true
}

func firstHandlingClient(lspManager *lsp.Manager, absPath string) *lsp.Client {
	for client := range lspManager.Clients().Seq() {
		if client.HandlesFile(absPath) {
			return client
		}
	}
	return nil
}

func formatLocations(kind string, locations []protocol.Location, workingDir string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "Found %d %s location(s):\n\n", len(locations), kind)
	for _, loc := range locations {
		path, err := loc.URI.Path()
		if err != nil {
			continue
		}
		fmt.Fprintf(&output, "%s:%d:%d\n", FormatToolPath(path, workingDir), loc.Range.Start.Line+1, loc.Range.Start.Character+1)
	}
	return strings.TrimSpace(output.String())
}

type documentSymbolEntry struct {
	Name     string
	Kind     string
	Line     uint32
	Column   uint32
	Children []documentSymbolEntry
}

func formatDocumentSymbols(filePath, workingDir string, symbols []documentSymbolEntry) string {
	var output strings.Builder
	fmt.Fprintf(&output, "%s\n", FormatToolPath(filePath, workingDir))
	for _, symbol := range symbols {
		writeDocumentSymbol(&output, symbol, 0)
	}
	return strings.TrimSpace(output.String())
}

func writeDocumentSymbol(output *strings.Builder, symbol documentSymbolEntry, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(output, "%s- %s (%s) %d:%d\n", indent, symbol.Name, symbol.Kind, symbol.Line, symbol.Column)
	for _, child := range symbol.Children {
		writeDocumentSymbol(output, child, depth+1)
	}
}

type workspaceSymbolEntry struct {
	Name   string
	Kind   string
	Path   string
	Line   uint32
	Column uint32
}

func formatWorkspaceSymbols(symbols []workspaceSymbolEntry, workingDir string) string {
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Path != symbols[j].Path {
			return symbols[i].Path < symbols[j].Path
		}
		if symbols[i].Line != symbols[j].Line {
			return symbols[i].Line < symbols[j].Line
		}
		return symbols[i].Name < symbols[j].Name
	})

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d workspace symbol(s):\n\n", len(symbols))
	for _, symbol := range symbols {
		fmt.Fprintf(&output, "%s (%s) %s:%d:%d\n", symbol.Name, symbol.Kind, FormatToolPath(symbol.Path, workingDir), symbol.Line, symbol.Column)
	}
	return strings.TrimSpace(output.String())
}

func symbolKindString(kind protocol.SymbolKind) string {
	if kind == 0 {
		return "unknown"
	}
	return strings.ToLower(fmt.Sprintf("%v", kind))
}

func documentSymbolEntries(results []protocol.DocumentSymbolResult) []documentSymbolEntry {
	entries := make([]documentSymbolEntry, 0, len(results))
	for _, result := range results {
		entry := documentSymbolEntry{
			Name:   result.GetName(),
			Kind:   documentSymbolKind(result),
			Line:   result.GetRange().Start.Line + 1,
			Column: result.GetRange().Start.Character + 1,
		}
		if symbol, ok := result.(*protocol.DocumentSymbol); ok {
			entry.Children = documentSymbolEntries(documentSymbolResultSlice(symbol.Children))
		}
		entries = append(entries, entry)
	}
	return entries
}

func documentSymbolKind(result protocol.DocumentSymbolResult) string {
	switch symbol := result.(type) {
	case *protocol.DocumentSymbol:
		return symbolKindString(symbol.Kind)
	case *protocol.SymbolInformation:
		return symbolKindString(symbol.Kind)
	default:
		return "unknown"
	}
}

func documentSymbolResultSlice(symbols []protocol.DocumentSymbol) []protocol.DocumentSymbolResult {
	results := make([]protocol.DocumentSymbolResult, 0, len(symbols))
	for i := range symbols {
		results = append(results, &symbols[i])
	}
	return results
}

func workspaceSymbolEntries(results []protocol.WorkspaceSymbolResult) []workspaceSymbolEntry {
	entries := make([]workspaceSymbolEntry, 0, len(results))
	for _, result := range results {
		loc := result.GetLocation()
		path, err := loc.URI.Path()
		if err != nil {
			continue
		}
		entries = append(entries, workspaceSymbolEntry{
			Name:   result.GetName(),
			Kind:   workspaceSymbolKind(result),
			Path:   path,
			Line:   loc.Range.Start.Line + 1,
			Column: loc.Range.Start.Character + 1,
		})
	}
	return entries
}

func workspaceSymbolKind(result protocol.WorkspaceSymbolResult) string {
	switch symbol := result.(type) {
	case *protocol.WorkspaceSymbol:
		return symbolKindString(symbol.Kind)
	case *protocol.SymbolInformation:
		return symbolKindString(symbol.Kind)
	default:
		return "unknown"
	}
}
