package hindsight

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetrieverUsesRemoteOnlyRecall(t *testing.T) {
	t.Parallel()

	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer token-1", r.Header.Get("Authorization"))

		var req RecallRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Contains(t, req.Query, "project context")

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "mem-1", "text": "Use SQLite for local storage.", "type": "decision"},
			},
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL+"/", "bank-1", "token-1"))
	recall, err := retriever.Recall(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "/v1/default/banks/bank-1/memories/recall", seenPath)
	require.Contains(t, recall, "<hindsight_memories>")
	require.Contains(t, recall, "Use SQLite for local storage.")
}

func TestRetrieverRetrieveQueriesHindsight(t *testing.T) {
	t.Parallel()

	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/memories/recall", r.URL.Path)
		var req RecallRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		gotQuery = req.Query
		require.Equal(t, "high", req.Budget)
		require.Equal(t, 256, req.MaxTokens)

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"id":           "remote-1",
					"text":         "Remote pitfall: avoid stale generated schemas.",
					"type":         "pitfall",
					"mentioned_at": time.Now().Format(time.RFC3339),
				},
			},
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL, "", ""))
	events, err := retriever.Retrieve(context.Background(), "schema pitfall", map[string]any{
		"budget":     "high",
		"max_tokens": 256,
	})
	require.NoError(t, err)
	require.Equal(t, "schema pitfall", gotQuery)
	require.Len(t, events, 1)
	require.Equal(t, "hindsight:remote-1", events[0].ID)
	require.Equal(t, "pitfall", string(events[0].Kind))
	require.Contains(t, events[0].Content, "stale generated schemas")
}

func TestRetrieverRetrieveForwardsFiltersAsTags(t *testing.T) {
	t.Parallel()

	var gotReq RecallRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/memories/recall", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))

		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "remote-1", "text": "Filtered project pitfall.", "type": "pitfall"},
			},
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL, "", ""))
	events, err := retriever.Retrieve(context.Background(), "", map[string]any{
		"scope":      "project",
		"kind":       "pitfall",
		"session_id": "sess-1",
		"tags":       []string{"repo:crush"},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Contains(t, gotReq.Query, "project context")
	require.Equal(t, "all", gotReq.TagsMatch)
	require.ElementsMatch(t, []string{
		"repo:crush",
		"scope:project",
		"kind:pitfall",
		"session:sess-1",
	}, gotReq.Tags)
}

func TestRetrieverReflectDoesNotFallbackToLocal(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/reflect", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"text": "Remote synthesis only.",
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL, "", ""))
	text, err := retriever.Reflect(context.Background(), "what do we know?", nil)
	require.NoError(t, err)
	require.Equal(t, "Remote synthesis only.", text)
}
