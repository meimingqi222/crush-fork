package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/crush/internal/lsp"
)

// resolvedSymbol holds the result of resolving a symbol name to an LSP position.
type resolvedSymbol struct {
	client *lsp.Client
	path   string
	line   int
	char   int
}

// resolveSymbol greps for a symbol name, triggers lazy LSP startup, and
// returns the first match position that a running LSP client confirms
// is a valid identifier. Matches inside comments or strings are skipped
// automatically because the LSP will reject them.
func resolveSymbol(ctx context.Context, lspManager *lsp.Manager, symbol, workingDir string) (*resolvedSymbol, error) {
	results, err := resolveSymbolResults(ctx, lspManager, symbol, workingDir)
	if err != nil {
		return nil, err
	}

	// Try each candidate until the LSP confirms it's a real identifier.
	// This filters out grep matches in comments, strings, or partial
	// identifiers that slipped past the word-boundary filter.
	for _, r := range results {
		_, err := r.client.FindDefinition(ctx, r.path, r.line, r.char)
		if err == nil || !isNoIdentifierError(err) {
			return r, nil
		}
	}
	// All candidates were rejected by the LSP; return the first one
	// so the caller gets a meaningful error from their own LSP call.
	return results[0], nil
}

// resolveSymbolResults greps for a symbol and returns all viable
// {client, path, position} tuples. Callers that need just one match
// (definition, rename, call hierarchy) use resolveSymbol; callers that
// want to iterate all matches (references) use this directly.
func resolveSymbolResults(ctx context.Context, lspManager *lsp.Manager, symbol, workingDir string) ([]*resolvedSymbol, error) {
	lspManager.Start(ctx, workingDir)

	// Use word boundaries to avoid matching inside larger identifiers
	// (e.g. "Bar" inside "myBar"). The symbol is already QuoteMeta'd
	// so dots and other regex metacharacters are escaped.
	pattern := `\b` + regexp.QuoteMeta(symbol) + `\b`
	matches, err := searchFilesWithRegex(pattern, workingDir, "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to search for symbol: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("symbol '%s' not found in grep results", symbol)
	}

	var results []*resolvedSymbol
	for _, match := range matches {
		absPath, err := filepath.Abs(match.path)
		if err != nil {
			continue
		}

		client := firstHandlingClient(lspManager, absPath)
		if client == nil {
			continue
		}

		results = append(results, &resolvedSymbol{
			client: client,
			path:   absPath,
			line:   match.lineNum,
			char:   match.charNum + getSymbolOffset(symbol),
		})
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no LSP client handles any file matching '%s'", symbol)
	}
	return results, nil
}

// isNoIdentifierError checks if an error indicates the grep match was not
// actually an identifier (e.g., matched inside a comment or string).
func isNoIdentifierError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no identifier found")
}
