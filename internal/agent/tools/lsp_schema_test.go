package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// dispatchedLSPOps extracts the op strings the lsp tool actually handles by
// parsing the switch in NewLSPTool. Reading the source keeps this test honest
// even though the dispatch table is not exported.
func dispatchedLSPOps(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lsp.go", nil, 0)
	require.NoError(t, err)

	var ops []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewLSPTool" {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			sw, ok := inner.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			// Only the op switch selects on params.Op.
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Op" {
				return true
			}
			for _, stmt := range sw.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					lit, ok := expr.(*ast.BasicLit)
					if ok && lit.Kind == token.STRING {
						ops = append(ops, strings.Trim(lit.Value, `"`))
					}
				}
			}
			return false
		})
		return false
	})

	require.NotEmpty(t, ops, "failed to extract dispatched ops")
	return ops
}

// opsFromDescription pulls the advertised op list out of the LSPParams.Op
// struct tag, which is what the model actually receives in the JSON schema.
func opsFromDescription(t *testing.T) []string {
	t.Helper()

	field, ok := reflect.TypeOf(LSPParams{}).FieldByName("Op")
	require.True(t, ok)
	desc := field.Tag.Get("description")
	require.NotEmpty(t, desc)

	_, list, found := strings.Cut(desc, "One of:")
	require.True(t, found, "Op description must enumerate ops after %q", "One of:")

	var ops []string
	for _, raw := range strings.Split(strings.TrimSuffix(strings.TrimSpace(list), "."), ",") {
		if op := strings.TrimSpace(raw); op != "" {
			ops = append(ops, op)
		}
	}
	return ops
}

// TestLSPOpSchemaMatchesDispatch pins the JSON schema against the real
// dispatch table. These drifted apart once already: five removed ops
// (declaration, implementation, type_definition, hover, format) stayed in the
// schema while two new ones (replace_symbol, call_hierarchy) were missing, so
// the model was told to call ops that return "unknown lsp op" and never
// learned about the ops that exist.
func TestLSPOpSchemaMatchesDispatch(t *testing.T) {
	t.Parallel()

	dispatched := dispatchedLSPOps(t)
	advertised := opsFromDescription(t)

	slices.Sort(dispatched)
	slices.Sort(advertised)
	require.Equal(t, dispatched, advertised,
		"LSPParams.Op description must list exactly the dispatched ops")
}

// TestLSPUnknownOpErrorListsRealOps keeps the fallback error message aligned
// with the dispatch table too — it is the model's recovery hint.
func TestLSPUnknownOpErrorListsRealOps(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool(nil, nil, t.TempDir())
	desc := tool.Info().Description
	require.NotEmpty(t, desc)

	for _, op := range dispatchedLSPOps(t) {
		require.Contains(t, lspUnknownOpMessage, op,
			"unknown-op error must mention %q", op)
	}
	for _, removed := range []string{"declaration", "implementation", "type_definition", "hover", "format"} {
		require.NotRegexp(t, regexp.MustCompile(`\b`+removed+`\b`), lspUnknownOpMessage,
			"unknown-op error still advertises removed op %q", removed)
	}
}
