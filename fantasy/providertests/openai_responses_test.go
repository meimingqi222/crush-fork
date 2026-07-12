package providertests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/x/vcr"
	"github.com/stretchr/testify/require"
)

type openAIResponsesVCRTransport struct {
	next http.RoundTripper
}

func (t openAIResponsesVCRTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return t.next.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()

	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if input, ok := payload["input"].([]any); ok {
			filtered := input[:0]
			for _, item := range input {
				value, _ := item.(map[string]any)
				if value["type"] != "reasoning" {
					filtered = append(filtered, item)
				}
			}
			payload["input"] = filtered
			if normalized, marshalErr := json.Marshal(payload); marshalErr == nil {
				body = normalized
			}
		}
	}

	clone := req.Clone(req.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	return t.next.RoundTrip(clone)
}

func TestOpenAIResponsesCommon(t *testing.T) {
	var pairs []builderPair
	for _, m := range openaiTestModels {
		pairs = append(pairs, builderPair{m.name, openAIReasoningBuilder(m.model), nil, nil})
	}
	testCommon(t, pairs)
}

func openAIReasoningBuilder(model string) builderFunc {
	return func(t *testing.T, r *vcr.Recorder) (fantasy.LanguageModel, error) {
		provider, err := openai.New(
			openai.WithAPIKey(os.Getenv("FANTASY_OPENAI_API_KEY")),
			// Reasoning items contain opaque encrypted replay state derived from
			// the cassette response. Provider unit tests verify that this state is
			// serialized; excluding it from VCR matching keeps these integration
			// cassettes focused on stable request fields and response parsing.
			openai.WithHTTPClient(&http.Client{Transport: openAIResponsesVCRTransport{next: r}}),
			openai.WithUseResponsesAPI(),
		)
		if err != nil {
			return nil, err
		}
		return provider.LanguageModel(t.Context(), model)
	}
}

func TestOpenAIResponsesWithSummaryThinking(t *testing.T) {
	opts := fantasy.ProviderOptions{
		openai.Name: &openai.ResponsesProviderOptions{
			Include: []openai.IncludeType{
				openai.IncludeReasoningEncryptedContent,
			},
			ReasoningEffort:  openai.ReasoningEffortOption(openai.ReasoningEffortHigh),
			ReasoningSummary: new("auto"),
		},
	}
	var pairs []builderPair
	for _, m := range openaiTestModels {
		if !m.reasoning {
			continue
		}
		pairs = append(pairs, builderPair{m.name, openAIReasoningBuilder(m.model), opts, nil})
	}
	testThinking(t, pairs, testOpenAIResponsesThinkingWithSummaryThinking)
}

func TestOpenAIResponsesObjectGeneration(t *testing.T) {
	var pairs []builderPair
	for _, m := range openaiTestModels {
		pairs = append(pairs, builderPair{m.name, openAIReasoningBuilder(m.model), nil, nil})
	}
	testObjectGeneration(t, pairs)
}

func testOpenAIResponsesThinkingWithSummaryThinking(t *testing.T, result *fantasy.AgentResult) {
	reasoningContentCount := 0
	encryptedData := 0
	// Test if we got the signature
	for _, step := range result.Steps {
		for _, msg := range step.Messages {
			for _, content := range msg.Content {
				if content.GetType() == fantasy.ContentTypeReasoning {
					reasoningContentCount += 1
					reasoningContent, ok := fantasy.AsContentType[fantasy.ReasoningPart](content)
					if !ok {
						continue
					}
					if len(reasoningContent.ProviderOptions) == 0 {
						continue
					}

					openaiReasoningMetadata, ok := reasoningContent.ProviderOptions[openai.Name]
					if !ok {
						continue
					}
					if typed, ok := openaiReasoningMetadata.(*openai.ResponsesReasoningMetadata); ok {
						require.NotEmpty(t, typed.EncryptedContent)
						encryptedData += 1
					}
				}
			}
		}
	}
	require.Greater(t, reasoningContentCount, 0)
	require.Greater(t, encryptedData, 0)
	require.Equal(t, reasoningContentCount, encryptedData)
}
