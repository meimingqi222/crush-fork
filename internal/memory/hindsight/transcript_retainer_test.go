package hindsight

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTranscriptRetainerRetainsWindow(t *testing.T) {
	t.Parallel()

	var gotReq struct {
		Items []RetainItem `json:"items"`
		Async bool         `json:"async"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/default/banks/crush/memories", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	retainer := NewTranscriptRetainer(
		NewClient(server.URL, "", ""),
		WithRetainTags([]string{"project:crush"}),
	)
	err := retainer.RetainTranscript(context.Background(), "sess-1", 3, "USER: hi")
	require.NoError(t, err)
	require.True(t, gotReq.Async)
	require.Len(t, gotReq.Items, 1)
	require.Equal(t, "USER: hi", gotReq.Items[0].Content)
	require.Equal(t, "transcript:sess-1:3", gotReq.Items[0].DocumentID)
	require.Contains(t, gotReq.Items[0].Tags, "scope:session")
	require.Contains(t, gotReq.Items[0].Tags, "kind:transcript_window")
	require.Contains(t, gotReq.Items[0].Tags, "session:sess-1")
	require.Contains(t, gotReq.Items[0].Tags, "project:crush")
}

func TestTranscriptRetainerRejectsNegativeTurnCount(t *testing.T) {
	t.Parallel()

	retainer := NewTranscriptRetainer(NewClient("http://example.com", "", ""))
	err := retainer.RetainTranscript(context.Background(), "sess-1", -1, "USER: hi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "turn count")
}
