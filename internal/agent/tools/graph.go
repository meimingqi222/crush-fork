package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed graph.md
var graphDescription []byte

const GraphToolName = "graph"

// GraphParams covers both query modes over the knowledge-graph triple store.
// "path" traverses edges from seed IDs (formerly graph_query); "triples"
// performs a structured subject/predicate lookup (formerly triple_query).
// Both modes read from the same TripleStore, so they are merged into one
// tool rather than presenting the LLM with two tools over one dataset.
type GraphParams struct {
	Mode string `json:"mode" description:"Query mode: \"path\" (traverse edges from seed IDs) or \"triples\" (structured subject/predicate lookup). Defaults to \"path\"."`

	// path mode
	SeedIDs   []string `json:"seed_ids,omitempty" description:"Starting memory event or triple IDs to traverse from (path mode, required)"`
	MaxHops   int      `json:"max_hops,omitempty" description:"Maximum traversal depth, default 2 (path mode)"`
	EdgeTypes []string `json:"edge_types,omitempty" description:"Optional edge type filters: related_to, contradicts, refines, depends_on (path mode)"`

	// triples mode
	Subject   string `json:"subject,omitempty" description:"Filter by subject, exact match (triples mode)"`
	Predicate string `json:"predicate,omitempty" description:"Filter by predicate, exact match (triples mode)"`
	Limit     int    `json:"limit,omitempty" description:"Maximum number of triples to return, default 50 (triples mode)"`
}

// TripleStoreGraphQuerier is satisfied by *engine.TripleStore.
type TripleStoreGraphQuerier interface {
	GraphQuery(ctx context.Context, seedIDs []string, maxHops int, edgeTypes []engine.EdgeType) ([]engine.MemoryEvent, []engine.Triple, error)
	QueryTriples(ctx context.Context, subject, predicate string, limit int) ([]engine.Triple, error)
}

func NewGraphTool(tripleStore TripleStoreGraphQuerier) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GraphToolName,
		string(graphDescription),
		func(ctx context.Context, params GraphParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if tripleStore == nil {
				return fantasy.NewTextErrorResponse("Memory knowledge graph is not available. Enable a memory backend with triple-store support to use graph."), nil
			}

			switch params.Mode {
			case "triples":
				return runGraphTriplesQuery(ctx, tripleStore, params)
			case "path", "":
				if len(params.SeedIDs) == 0 {
					return fantasy.NewTextErrorResponse("At least one seed_id is required for mode=\"path\" traversal."), nil
				}
				return runGraphPathQuery(ctx, tripleStore, params)
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Unknown graph mode %q. Use \"path\" or \"triples\".", params.Mode)), nil
			}
		},
	)
}

func runGraphPathQuery(ctx context.Context, tripleStore TripleStoreGraphQuerier, params GraphParams) (fantasy.ToolResponse, error) {
	maxHops := params.MaxHops
	if maxHops <= 0 {
		maxHops = 2
	}

	var edgeTypes []engine.EdgeType
	for _, et := range params.EdgeTypes {
		edgeTypes = append(edgeTypes, engine.EdgeType(et))
	}

	events, triples, err := tripleStore.GraphQuery(ctx, params.SeedIDs, maxHops, edgeTypes)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Graph query failed: %s", err.Error())), nil
	}

	if len(events) == 0 && len(triples) == 0 {
		return fantasy.NewTextResponse("No connected memories found from the given seeds."), nil
	}

	var b strings.Builder
	if len(events) > 0 {
		b.WriteString(fmt.Sprintf("Connected Events (%d):\n", len(events)))
		b.WriteString(formatMemoryEvents(events))
	}
	if len(triples) > 0 {
		b.WriteString(fmt.Sprintf("\nConnected Triples (%d):\n", len(triples)))
		for _, t := range triples {
			b.WriteString(fmt.Sprintf("  %s — %s — %s (confidence: %.0f%%)\n",
				t.Subject, t.Predicate, t.Object, t.Confidence*100))
		}
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func runGraphTriplesQuery(ctx context.Context, tripleStore TripleStoreGraphQuerier, params GraphParams) (fantasy.ToolResponse, error) {
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
}
