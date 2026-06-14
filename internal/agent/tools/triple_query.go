package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed triple_query.md
var tripleQueryDescription []byte

const TripleQueryToolName = "triple_query"

type TripleQueryParams struct {
	Subject   string `json:"subject,omitempty" description:"Filter by subject (exact match, omit for wildcard)"`
	Predicate string `json:"predicate,omitempty" description:"Filter by predicate (exact match, omit for wildcard)"`
	Limit     int    `json:"limit,omitempty" description:"Maximum number of triples to return (default 50)"`
}

// TripleStoreQuerier is satisfied by *engine.TripleStore.
type TripleStoreQuerier interface {
	QueryTriples(ctx context.Context, subject, predicate string, limit int) ([]engine.Triple, error)
}

func NewTripleQueryTool(tripleStore TripleStoreQuerier) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TripleQueryToolName,
		string(tripleQueryDescription),
		func(ctx context.Context, params TripleQueryParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if tripleStore == nil {
				return fantasy.NewTextErrorResponse("Memory triple store is not available. Enable the memory engine to use triple_query."), nil
			}

			limit := params.Limit
			if limit <= 0 {
				limit = 50
			}

			triples, err := tripleStore.QueryTriples(ctx, params.Subject, params.Predicate, limit)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Triple query failed: %s", err.Error())), nil
			}

			if len(triples) == 0 {
				return fantasy.NewTextResponse("No matching triples found."), nil
			}

			var b strings.Builder
			b.WriteString(fmt.Sprintf("Found %d triple(s):\n\n", len(triples)))
			for _, t := range triples {
				b.WriteString(fmt.Sprintf("  %s — %s — %s\n", t.Subject, t.Predicate, t.Object))
				b.WriteString(fmt.Sprintf("    Confidence: %.0f%% | Scope: %s", t.Confidence*100, t.Scope))
				if t.Veracity != "" {
					b.WriteString(fmt.Sprintf(" | Veracity: %s", t.Veracity))
				}
				if t.SourceEventID != "" {
					b.WriteString(fmt.Sprintf(" | Source: %s", t.SourceEventID))
				}
				b.WriteString("\n")
			}
			return fantasy.NewTextResponse(b.String()), nil
		},
	)
}
