package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestSourcegraphToolAppliesCountAfterFilteringFileMatches(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, "https://sourcegraph.com/.api/graphql", req.URL.String())
			body := `{"data":{"search":{"results":{"matchCount":3,"resultCount":3,"limitHit":false,"results":[{"__typename":"Repository","name":"repo/skip"},{"__typename":"FileMatch","repository":{"name":"repo/one"},"file":{"path":"one.go","url":"https://example.com/one","content":"package main\nfunc one() {}\n"},"lineMatches":[{"preview":"func one() {}","lineNumber":2}]},{"__typename":"FileMatch","repository":{"name":"repo/two"},"file":{"path":"two.go","url":"https://example.com/two","content":"package main\nfunc two() {}\n"},"lineMatches":[{"preview":"func two() {}","lineNumber":2}]}]}}}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	tool := NewSourcegraphTool(client)
	input, err := json.Marshal(SourcegraphParams{
		Query:         "func main",
		Count:         2,
		ContextWindow: 1,
	})
	require.NoError(t, err)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  SourcegraphToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "## Result 1: repo/one/one.go")
	require.Contains(t, resp.Content, "## Result 2: repo/two/two.go")
	require.NotContains(t, resp.Content, "No results found")
}
