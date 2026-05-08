package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/stretchr/testify/require"
)

type capturingTransport struct {
	captured [][]byte
	respond  func(n int, providerType string) *http.Response
}

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	c.captured = append(c.captured, body)
	providerType := "anthropic"
	if req.URL.Host == "api.openai.com" {
		providerType = "openai"
	} else if req.URL.Host == "openrouter.ai" {
		providerType = "openrouter"
	} else if req.URL.Host == "api.z.ai" {
		providerType = "zai"
	}
	resp := c.respond(len(c.captured)-1, providerType)
	resp.Request = req
	return resp, nil
}

func mockAnthropicSSE(text string) *http.Response {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(sseBody)),
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
	}
}

func mockOpenAISSE(text string) *http.Response {
	sseBody := `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{"content":"` + text + `"},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105}}` + "\n\n" +
		"data: [DONE]\n\n"
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       io.NopCloser(bytes.NewBufferString(sseBody)),
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
	}
}

func buildCaptureAgent(t *testing.T, env fakeEnv, httpClient *http.Client, makeProvider func(*http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error)) SessionAgent {
	t.Helper()

	large, small, err := makeProvider(httpClient)
	require.NoError(t, err)

	fixedTime := func() time.Time {
		tt, _ := time.Parse("1/2/2006", "1/1/2025")
		return tt
	}
	p, err := coderPrompt(
		prompt.WithTimeFunc(fixedTime),
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(filepath.ToSlash(env.workingDir)),
		prompt.WithDisableGlobalContextFile(true),
	)
	require.NoError(t, err)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Options.Attribution = &config.Attribution{
		TrailerStyle:  "co-authored-by",
		GeneratedWith: true,
	}
	cfg.Config().Options.SkillsPaths = nil
	cfg.Config().Options.ContextPaths = nil
	cfg.Config().LSP = nil

	plugin.Register(&testTodoReminderPlugin{})
	err = plugin.Init(context.Background(), plugin.PluginInput{
		Config:     cfg,
		Sessions:   env.sessions,
		Messages:   env.messages,
		WorkingDir: env.workingDir,
	})
	require.NoError(t, err)

	systemPrompt, err := p.Build(context.TODO(), large.Provider(), large.Model(), cfg)
	require.NoError(t, err)
	systemPrompt = normalizeCoderPromptForFixtures(systemPrompt)

	modelName := large.Model()
	allTools := []fantasy.AgentTool{
		agenttools.NewBashToolWithSessions(env.sessions, env.permissions, env.workingDir, cfg.Config().Options.Attribution, modelName, nil),
		agenttools.NewDownloadTool(env.permissions, env.workingDir, httpClient),
		agenttools.NewEditTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
		agenttools.NewFetchTool(env.permissions, env.workingDir, httpClient),
		agenttools.NewGlobTool(env.workingDir),
		agenttools.NewGrepTool(env.workingDir, cfg.Config().Tools.Grep),
		agenttools.NewSourcegraphTool(httpClient),
		agenttools.NewViewTool(nil, env.permissions, *env.filetracker, env.workingDir, cfg.Config().Tools.Ls),
		agenttools.NewWriteTool(nil, env.permissions, env.history, *env.filetracker, env.workingDir),
	}

	sa := NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{
			Model: large,
			CatwalkCfg: catwalk.Model{
				ContextWindow:    200000,
				DefaultMaxTokens: 10000,
			},
		},
		SmallModel: Model{
			Model: small,
			CatwalkCfg: catwalk.Model{
				ContextWindow:    200000,
				DefaultMaxTokens: 10000,
			},
		},
		SystemPrompt: systemPrompt,
		WorkingDir:   env.workingDir,
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        allTools,
	})
	if sa2, ok := sa.(*sessionAgent); ok {
		sa2.disableAutoSummarize = true
	}
	return sa
}

func mockAnthropicToolCallSSE(toolName, toolID, toolInput string) *http.Response {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_tool","type":"message","role":"assistant","content":[],"model":"test","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + toolID + `","name":"` + toolName + `","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + toolInput + `}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":10}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(sseBody)),
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
	}
}

func mockOpenAIToolCallSSE(toolName, toolID, toolInputJSON string) *http.Response {
	// toolInputJSON is the raw JSON object; encode it as a JSON string for arguments field.
	argsEncoded, _ := json.Marshal(toolInputJSON)
	sseBody := `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + toolID + `","type":"function","function":{"name":"` + toolName + `","arguments":""}}]},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + string(argsEncoded) + `}}]},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234567890,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}` + "\n\n" +
		"data: [DONE]\n\n"
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       io.NopCloser(bytes.NewBufferString(sseBody)),
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
	}
}

// TestDumpProviderRequests captures what the agent sends for all providers.
// Run with: go test ./internal/agent/ -run TestDumpProviderRequests -v
func TestDumpProviderRequests(t *testing.T) {
	providers := []struct {
		name         string
		largeModelID string
		smallModelID string
		makeProvider func(*http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error)
		mockResp     func(n int, providerType string) *http.Response
	}{
		{
			name:         "anthropic",
			largeModelID: "claude-sonnet-4-6",
			smallModelID: "claude-haiku-4-5-20251001",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := anthropic.New(
					anthropic.WithAPIKey("test-key"),
					anthropic.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "claude-sonnet-4-6")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "claude-haiku-4-5-20251001")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				return mockAnthropicSSE("Hello! How can I help?")
			},
		},
		{
			name:         "openai",
			largeModelID: "gpt-5",
			smallModelID: "gpt-4o",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openai.New(
					openai.WithAPIKey("test-key"),
					openai.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "gpt-5")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "gpt-4o")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				return mockOpenAISSE("Hi")
			},
		},
		{
			name:         "openrouter",
			largeModelID: "moonshotai/kimi-k2-0905",
			smallModelID: "qwen/qwen3-next-80b-a3b-instruct",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openrouter.New(
					openrouter.WithAPIKey("test-key"),
					openrouter.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "moonshotai/kimi-k2-0905")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "qwen/qwen3-next-80b-a3b-instruct")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				return mockOpenAISSE("Hi")
			},
		},
		{
			name:         "zai",
			largeModelID: "glm-4.6",
			smallModelID: "glm-4.5-air",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openaicompat.New(
					openaicompat.WithBaseURL("https://api.z.ai/api/coding/paas/v4"),
					openaicompat.WithAPIKey("test-key"),
					openaicompat.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "glm-4.6")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "glm-4.5-air")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				return mockOpenAISSE("Hi")
			},
		},
	}

	for _, prov := range providers {
		t.Run(prov.name, func(t *testing.T) {
			ct := &capturingTransport{
				respond: prov.mockResp,
			}
			httpClient := &http.Client{Transport: ct}

			env := testEnv(t)
			sa := buildCaptureAgent(t, env, httpClient, prov.makeProvider)

			sess, err := env.sessions.Create(t.Context(), "New Session")
			require.NoError(t, err)

			_, err = sa.Run(t.Context(), SessionAgentCall{
				Prompt:          "Hello",
				SessionID:       sess.ID,
				MaxOutputTokens: 10000,
				NonInteractive:  true,
			})
			require.NoError(t, err)

			t.Logf("Captured %d requests for %s", len(ct.captured), prov.name)
			for i, body := range ct.captured {
				outFile := filepath.Join(os.TempDir(), "crush_capture_"+prov.name+"_req"+string(rune('0'+i))+".json")
				_ = os.WriteFile(outFile, body, 0o644)
				t.Logf("Request %d saved to %s", i, outFile)
			}
		})
	}
}

// TestDumpProviderMultiTurnRequests captures two-turn (tool call) interactions.
// Run with: go test ./internal/agent/ -run TestDumpProviderMultiTurnRequests -v
func TestDumpProviderMultiTurnRequests(t *testing.T) {
	const bashToolInput = `{"command":"echo hello","description":"say hello"}`
	const bashToolID = "tool_test1"

	providers := []struct {
		name         string
		makeProvider func(*http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error)
		mockResp     func(n int, providerType string) *http.Response
	}{
		{
			name: "anthropic",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := anthropic.New(
					anthropic.WithAPIKey("test-key"),
					anthropic.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "claude-sonnet-4-6")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "claude-haiku-4-5-20251001")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				if n == 0 {
					// Anthropic partial_json is the raw JSON object (no extra quoting).
					return mockAnthropicToolCallSSE("bash", bashToolID, bashToolInput)
				}
				return mockAnthropicSSE("Done")
			},
		},
		{
			name: "openai",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openai.New(
					openai.WithAPIKey("test-key"),
					openai.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "gpt-5")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "gpt-4o")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				if n == 0 {
					return mockOpenAIToolCallSSE("bash", bashToolID, bashToolInput)
				}
				return mockOpenAISSE("Done")
			},
			},
			{
			name: "openrouter",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openrouter.New(
					openrouter.WithAPIKey("test-key"),
					openrouter.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "moonshotai/kimi-k2-0905")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "qwen/qwen3-next-80b-a3b-instruct")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				if n == 0 {
					return mockOpenAIToolCallSSE("bash", bashToolID, bashToolInput)
				}
				return mockOpenAISSE("Done")
			},
			},
			{
			name: "zai",
			makeProvider: func(client *http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error) {
				p, err := openaicompat.New(
					openaicompat.WithBaseURL("https://api.z.ai/api/coding/paas/v4"),
					openaicompat.WithAPIKey("test-key"),
					openaicompat.WithHTTPClient(client),
				)
				if err != nil {
					return nil, nil, err
				}
				large, err := p.LanguageModel(context.Background(), "glm-4.6")
				if err != nil {
					return nil, nil, err
				}
				small, err := p.LanguageModel(context.Background(), "glm-4.5-air")
				if err != nil {
					return nil, nil, err
				}
				return large, small, nil
			},
			mockResp: func(n int, _ string) *http.Response {
				if n == 0 {
					return mockOpenAIToolCallSSE("bash", bashToolID, bashToolInput)
				}
				return mockOpenAISSE("Done")
			},
		},
	}

	for _, prov := range providers {
		t.Run(prov.name, func(t *testing.T) {
			ct := &capturingTransport{
				respond: prov.mockResp,
			}
			httpClient := &http.Client{Transport: ct}

			env := testEnv(t)
			sa := buildCaptureAgent(t, env, httpClient, prov.makeProvider)

			sess, err := env.sessions.Create(t.Context(), "New Session")
			require.NoError(t, err)

			_, err = sa.Run(t.Context(), SessionAgentCall{
				Prompt:          "use bash to create a file named test.txt with content 'hello bash'. do not print anything",
				SessionID:       sess.ID,
				MaxOutputTokens: 10000,
				NonInteractive:  true,
			})
			require.NoError(t, err)

			t.Logf("Captured %d requests for %s", len(ct.captured), prov.name)
			for i, body := range ct.captured {
				outFile := filepath.Join(os.TempDir(), "crush_capture_multiturn_"+prov.name+"_req"+string(rune('0'+i))+".json")
				_ = os.WriteFile(outFile, body, 0o644)
				t.Logf("Request %d saved to %s", i, outFile)
			}
		})
	}
}
