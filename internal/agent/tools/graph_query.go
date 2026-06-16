package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/memory/engine"
)

//go:embed graph_query.md
var graphQueryDescription []byte

const GraphQueryToolName = "graph_query"

type GraphQueryParams struct {
	SeedIDs   []string `json:"seed_ids" description:"Starting memory event or triple IDs to traverse from"`
	MaxHops   int      `json:"max_hops,omitempty" description:"Maximum traversal depth (default 2)"`
	EdgeTypes []string `json:"edge_types,omitempty" description:"Optional edge type filters: related_to, contradicts, refines, depends_on"`
}

// TripleStoreGraphQuerier is satisfied by *engine.TripleStore.
type TripleStoreGraphQuerier interface {
	GraphQuery(ctx context.Context, seedIDs []string, maxHops int, edgeTypes []engine.EdgeType) ([]engine.MemoryEvent, []engine.Triple, error)
}

func NewGraphQueryTool(tripleStore TripleStoreGraphQuerier) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		GraphQueryToolName,
		string(graphQueryDescription),
		func(ctx context.Context, params GraphQueryParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if tripleStore == nil {
				return fantasy.NewTextErrorResponse("Memory triple store is not available. Enable the memory engine to use graph_query."), nil
			}
			if len(params.SeedIDs) == 0 {
				return fantasy.NewTextErrorResponse("At least one seed_id is required for graph traversal."), nil
			}

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
		},
	)
}
