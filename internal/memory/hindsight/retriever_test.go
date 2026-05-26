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
	var gotReqs []RecallRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer token-1", r.Header.Get("Authorization"))

		var gotReq RecallRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		require.Contains(t, gotReq.Query, "project context")
		gotReqs = append(gotReqs, gotReq)

		results := []map[string]any{
			{"id": "mem-1", "text": "Use SQLite for local storage.", "type": "decision"},
		}
		if len(gotReq.Tags) == 0 {
			results = []map[string]any{
				{"id": "global-1", "text": "Prefer concise answers.", "type": "preference"},
			}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"results": results,
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(
		NewClient(server.URL+"/", "bank-1", "token-1"),
		WithRecallTags([]string{"project:crush-abc123"}, "any"),
	)
	recall, err := retriever.Recall(context.Background(), map[string]any{"session_id": "sess-1"})
	require.NoError(t, err)
	require.Equal(t, "/v1/default/banks/bank-1/memories/recall", seenPath)
	require.Len(t, gotReqs, 2)
	require.Equal(t, []string{"project:crush-abc123"}, gotReqs[0].Tags)
	require.Equal(t, "any", gotReqs[0].TagsMatch)
	require.Empty(t, gotReqs[1].Tags)
	require.Contains(t, recall, "<hindsight_memories>")
	require.Contains(t, recall, "Use SQLite for local storage.")
	require.Contains(t, recall, "Prefer concise answers.")
}

func TestRetrieverRetrieveQueriesHindsight(t *testing.T) {
	t.Parallel()

	var gotQuery string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
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
	require.Equal(t, 1, calls)
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

	retriever := NewRetriever(
		NewClient(server.URL, "", ""),
		WithRecallTags([]string{"project:crush-abc123"}, "any"),
	)
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
		"project:crush-abc123",
		"repo:crush",
		"scope:project",
		"kind:pitfall",
	}, gotReq.Tags)
}

func TestRetrieverReflectDoesNotFallbackToLocal(t *testing.T) {
	t.Parallel()

	var gotReq ReflectRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/reflect", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"text": "Remote synthesis only.",
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(
		NewClient(server.URL, "", ""),
		WithRecallTags([]string{"project:crush-abc123"}, "any"),
	)
	text, err := retriever.Reflect(context.Background(), "what do we know?", nil)
	require.NoError(t, err)
	require.Equal(t, "Remote synthesis only.", text)
	require.Equal(t, []string{"project:crush-abc123"}, gotReq.Tags)
	require.Equal(t, "any", gotReq.TagsMatch)
}

func TestRetrieverLoadMentalModels(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "detail=content", r.URL.RawQuery)

		models := []map[string]any{
			{
				"id":      "mm-1",
				"bank_id": "crush",
				"name":    "Code Style Guidelines",
				"content": "Use gofumpt and standard style.",
			},
			{
				"id":      "mm-2",
				"bank_id": "crush",
				"name":    "Architecture Guidelines",
				"content": "Modular monolith with service isolation.",
			},
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"items": models,
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL, "", ""))

	// Test synchronous load
	err := retriever.LoadMentalModels(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/v1/default/banks/crush/mental-models", gotPath)

	snippet := retriever.MentalModelsSnippet()
	require.Contains(t, snippet, "<mental_models>")
	require.Contains(t, snippet, "Curated long-running summaries of this bank.")
	require.Contains(t, snippet, "# Architecture Guidelines")
	require.Contains(t, snippet, "Modular monolith with service isolation.")
	require.Contains(t, snippet, "# Code Style Guidelines")
	require.Contains(t, snippet, "Use gofumpt and standard style.")
	require.False(t, retriever.MentalModelsLoadedAt().IsZero())
}

func TestRetrieverLoadMentalModelsConcurrencyInFlight(t *testing.T) {
	t.Parallel()

	blockCh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh // block the API request to simulate long load
		models := []map[string]any{
			{
				"id":      "mm-1",
				"name":    "Blocking Model",
				"content": "Data is here.",
			},
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"items": models,
		}))
	}))
	defer server.Close()

	retriever := NewRetriever(NewClient(server.URL, "", ""))

	// Trigger first load in a goroutine
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		err := retriever.LoadMentalModels(context.Background())
		require.NoError(t, err)
	}()

	// Wait briefly to let the first load acquire lock and start http call
	time.Sleep(50 * time.Millisecond)

	// Trigger second load - should return immediately due to in-flight lock
	start := time.Now()
	err := retriever.LoadMentalModels(context.Background())
	require.NoError(t, err)
	require.Less(t, time.Since(start), 20*time.Millisecond, "second load should be non-blocking")

	// Release the HTTP server blocking and wait for the first load to finish
	close(blockCh)
	<-doneCh

	// Confirm first load has successfully saved the data
	require.Contains(t, retriever.MentalModelsSnippet(), "Blocking Model")
}
