package tools

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

// ReferencesParams describes a symbol reference search. It is retained even
// though the standalone references tool is unregistered: the UI layer renders
// historical reference tool messages and needs the type for JSON decoding.
type ReferencesParams struct {
	Symbol string `json:"symbol" description:"The symbol name to search for (e.g., function name, variable name, type name)"`
	Path   string `json:"path,omitempty" description:"The directory to search in. Use a directory/file to narrow down the symbol search. Defaults to the current working directory."`
}

const ReferencesToolName = "lsp_references"

func find(ctx context.Context, lspManager *lsp.Manager, symbol string, match grepMatch) ([]protocol.Location, error) {
	absPath, err := filepath.Abs(match.path)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %s", err)
	}

	var client *lsp.Client
	for c := range lspManager.Clients().Seq() {
		if c.HandlesFile(absPath) {
			client = c
			break
		}
	}

	if client == nil {
		slog.Warn("No LSP clients to handle", "path", match.path)
		return nil, nil
	}

	return client.FindReferences(
		ctx,
		absPath,
		match.lineNum,
		match.charNum+getSymbolOffset(symbol),
		true,
	)
}

// getSymbolOffset returns the character offset to the actual symbol name
// in a qualified symbol (e.g., "Bar" in "foo.Bar" or "method" in "Class::method").
func getSymbolOffset(symbol string) int {
	// Check for :: separator (Rust, C++, Ruby modules/classes, PHP static).
	if idx := strings.LastIndex(symbol, "::"); idx != -1 {
		return idx + 2
	}
	// Check for . separator (Go, Python, JavaScript, Java, C#, Ruby methods).
	if idx := strings.LastIndex(symbol, "."); idx != -1 {
		return idx + 1
	}
	// Check for \ separator (PHP namespaces).
	if idx := strings.LastIndex(symbol, "\\"); idx != -1 {
		return idx + 1
	}
	return 0
}

func cleanupLocations(locations []protocol.Location) []protocol.Location {
	slices.SortFunc(locations, func(a, b protocol.Location) int {
		if a.URI != b.URI {
			return strings.Compare(string(a.URI), string(b.URI))
		}
		if a.Range.Start.Line != b.Range.Start.Line {
			return cmp.Compare(a.Range.Start.Line, b.Range.Start.Line)
		}
		return cmp.Compare(a.Range.Start.Character, b.Range.Start.Character)
	})
	return slices.CompactFunc(locations, func(a, b protocol.Location) bool {
		return a.URI == b.URI &&
			a.Range.Start.Line == b.Range.Start.Line &&
			a.Range.Start.Character == b.Range.Start.Character
	})
}

func groupByFilename(locations []protocol.Location) map[string][]protocol.Location {
	files := make(map[string][]protocol.Location)
	for _, loc := range locations {
		path, err := loc.URI.Path()
		if err != nil {
			slog.Error("Failed to convert location URI to path", "uri", loc.URI, "error", err)
			continue
		}
		files[path] = append(files[path], loc)
	}
	return files
}

func formatReferences(locations []protocol.Location) string {
	fileRefs := groupByFilename(locations)
	files := slices.Collect(maps.Keys(fileRefs))
	sort.Strings(files)

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d reference(s) in %d file(s):\n\n", len(locations), len(files))

	for _, file := range files {
		refs := fileRefs[file]
		fmt.Fprintf(&output, "%s (%d reference(s)):\n", file, len(refs))
		for _, ref := range refs {
			line := ref.Range.Start.Line + 1
			char := ref.Range.Start.Character + 1
			fmt.Fprintf(&output, "  Line %d, Column %d\n", line, char)
		}
		output.WriteString("\n")
	}

	return output.String()
}
