package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/userinput"
	"github.com/stretchr/testify/require"
)

func runRequestUserInputTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params RequestUserInputParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Name: RequestUserInputToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

func TestRequestUserInputToolRequiresSessionID(t *testing.T) {
	t.Parallel()

	tool := NewRequestUserInputTool(userinput.NewService())
	input, err := json.Marshal(RequestUserInputParams{
		Questions: []RequestUserInputQuestion{{
			Header:   "Mode",
			ID:       "mode",
			Question: "Choose one",
			Options: []RequestUserInputOption{
				{Label: "A", Description: "Option A"},
				{Label: "B", Description: "Option B"},
			},
		}},
	})
	require.NoError(t, err)

	_, err = tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  RequestUserInputToolName,
		Input: string(input),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "session ID")
}

func TestRequestUserInputToolSubmitsOutsidePlanMode(t *testing.T) {
	t.Parallel()

	userInputSvc := userinput.NewService()
	tool := NewRequestUserInputTool(userInputSvc)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, SessionIDContextKey, "session-1")

	responses := make(chan fantasy.ToolResponse, 1)
	go func() {
		responses <- runRequestUserInputTool(t, tool, ctx, RequestUserInputParams{
			Questions: []RequestUserInputQuestion{{
				Header:   "Mode",
				ID:       "mode",
				Question: "Choose one",
				Options: []RequestUserInputOption{
					{Label: "A", Description: "Option A"},
					{Label: "B", Description: "Option B"},
				},
			}},
		})
	}()

	requestEvent := <-userInputSvc.Subscribe(ctx)
	req := requestEvent.Payload
	userInputSvc.Resolve(userinput.Response{
		RequestID:  req.ID,
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Status:     userinput.ResponseStatusSubmitted,
		Answers: []userinput.Answer{{
			QuestionID:     "mode",
			SelectedOption: "A",
		}},
	})

	resp := <-responses
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `"status":"submitted"`)
	require.Contains(t, resp.Content, `"selected_option":"A"`)
}
