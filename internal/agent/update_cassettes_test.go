// TestUpdateCassettes regenerates VCR cassette files for TestCoderAgent after
// tool changes (e.g., removing hashline_edit, ls, multiedit). Run with:
//
//	go test ./internal/agent/ -run TestUpdateCassettes -v -count=1
//
// This test loads old cassette responses, runs the agent using those responses
// as mocks to capture new request bodies, then writes updated cassettes.
// After running, TestCoderAgent should pass with the regenerated cassettes.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/memory"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

// ucCassette mirrors the on-disk VCR cassette YAML format (version 2).
// Duration is kept as interface{} to preserve the original string value
// (e.g., "580.853041ms") without re-serializing through time.Duration.
type ucCassette struct {
	Version      int               `yaml:"version"`
	Interactions []*ucInteraction  `yaml:"interactions"`
}

type ucInteraction struct {
	ID       int          `yaml:"id"`
	Request  ucRequest    `yaml:"request"`
	Response ucResponse   `yaml:"response"`
}

type ucRequest struct {
	Proto         string      `yaml:"proto"`
	ProtoMajor    int         `yaml:"proto_major"`
	ProtoMinor    int         `yaml:"proto_minor"`
	ContentLength int64       `yaml:"content_length"`
	Host          string      `yaml:"host"`
	Body          string      `yaml:"body,omitempty"`
	Headers       http.Header `yaml:"headers,omitempty"`
	URL           string      `yaml:"url"`
	Method        string      `yaml:"method"`
}

type ucResponse struct {
	Proto         string      `yaml:"proto"`
	ProtoMajor    int         `yaml:"proto_major"`
	ProtoMinor    int         `yaml:"proto_minor"`
	ContentLength int64       `yaml:"content_length"`
	Uncompressed  bool        `yaml:"uncompressed,omitempty"`
	Body          string      `yaml:"body"`
	Headers       http.Header `yaml:"headers"`
	Status        string      `yaml:"status"`
	Code          int         `yaml:"code"`
	Duration      interface{} `yaml:"duration,omitempty"`
}

// ucMarshal serializes the cassette using the same settings as charm.land/x/vcr.
func ucMarshal(c *ucCassette) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	enc.CompactSeqIndent()
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("ucMarshal: %w", err)
	}
	return buf.Bytes(), nil
}

// ucShouldKeep returns true for cassette interactions that should be kept:
// - response code must be 200
// - request body must NOT be a title-gen call (max_tokens == 40)
func ucShouldKeep(i *ucInteraction) bool {
	if i.Response.Code != 200 {
		return false
	}
	var req map[string]interface{}
	if err := json.Unmarshal([]byte(i.Request.Body), &req); err == nil {
		if mt, ok := req["max_tokens"].(float64); ok && mt == 40 {
			return false
		}
	}
	return true
}

// ucBuildHTTPResponse converts a ucResponse to an *http.Response suitable for
// use in a capturingTransport. A fresh io.Reader is created so the body can be
// consumed exactly once.
func ucBuildHTTPResponse(r ucResponse) *http.Response {
	return &http.Response{
		Status:        r.Status,
		StatusCode:    r.Code,
		Proto:         r.Proto,
		ProtoMajor:    r.ProtoMajor,
		ProtoMinor:    r.ProtoMinor,
		ContentLength: r.ContentLength,
		Uncompressed:  r.Uncompressed,
		Header:        r.Headers,
		Body:          io.NopCloser(strings.NewReader(r.Body)),
	}
}

// testEnvWithDir creates a fakeEnv with an explicit working directory instead
// of deriving it from t.Name(). Use this when the working dir must match a
// specific path for cassette system-prompt stability.
func testEnvWithDir(t *testing.T, workingDir string) fakeEnv {
	t.Helper()
	plugin.Reset()
	t.Cleanup(plugin.Reset)

	os.RemoveAll(workingDir)
	require.NoError(t, os.MkdirAll(workingDir, 0o755))

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	permissions := permission.NewPermissionService(workingDir, true, []string{})
	histSvc := history.NewService(q, conn)
	memSvc, err := memory.NewService(t.TempDir())
	require.NoError(t, err)
	ftSvc := filetracker.NewService(q)
	lspClients := csync.NewMap[string, *lsp.Client]()

	t.Cleanup(func() {
		conn.Close()
		os.RemoveAll(workingDir)
	})

	return fakeEnv{
		workingDir:  workingDir,
		sessions:    sessions,
		messages:    messages,
		permissions: permissions,
		history:     histSvc,
		memory:      memSvc,
		filetracker: &ftSvc,
		lspClients:  lspClients,
	}
}

// ucProviderSpec describes one of the four providers used in TestCoderAgent.
type ucProviderSpec struct {
	// dirName is the subdirectory name under testdata/TestCoderAgent/
	dirName string
	// makeProvider constructs (large, small) language models.
	makeProvider func(*http.Client) (fantasy.LanguageModel, fantasy.LanguageModel, error)
}

func ucProviders() []ucProviderSpec {
	return []ucProviderSpec{
		{
			dirName: "anthropic-sonnet",
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
		},
		{
			dirName: "openai-gpt-5",
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
		},
		{
			dirName: "openrouter-kimi-k2",
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
		},
		{
			dirName: "zai-glm4.6",
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
		},
	}
}

// ucTestCase maps a cassette file name (snake_case, no .yaml) to the user
// prompt used in TestCoderAgent for that sub-test.
type ucTestCase struct {
	cassetteName string // e.g. "bash_tool"
	prompt       string
}

func ucTestCases() []ucTestCase {
	return []ucTestCase{
		{
			cassetteName: "simple_test",
			prompt:       "Hello",
		},
		{
			cassetteName: "read_a_file",
			prompt:       "Read the go mod",
		},
		{
			cassetteName: "update_a_file",
			prompt:       "update the main.go file by changing the print to say hello from crush",
		},
		{
			cassetteName: "bash_tool",
			prompt:       "use bash to create a file named test.txt with content 'hello bash'. do not print its timestamp",
		},
		{
			cassetteName: "download_tool",
			prompt:       "download the file from https://example-files.online-convert.com/document/txt/example.txt and save it as example.txt",
		},
		{
			cassetteName: "fetch_tool",
			prompt:       "fetch the content from https://example-files.online-convert.com/website/html/example.html and tell me if it contains the word 'John Doe'",
		},
		{
			cassetteName: "glob_tool",
			prompt:       "use glob to find all .go files in the current directory",
		},
		{
			cassetteName: "grep_tool",
			prompt:       "use grep to search for the word 'package' in go files",
		},
		{
			cassetteName: "sourcegraph_tool",
			prompt:       "use sourcegraph to search for 'func main' in Go repositories",
		},
		{
			cassetteName: "write_tool",
			prompt:       `use write to create a new file called config.json with content '{"name": "test", "version": "1.0.0"}'`,
		},
	}
}

// TestUpdateCassettes regenerates all cassette files for TestCoderAgent.
// It uses the old cassette responses as mocks, runs the agent to capture the
// new request bodies, then writes updated cassettes. After this test passes,
// TestCoderAgent should pass with the regenerated cassettes.
func TestUpdateCassettes(t *testing.T) {
	// Delete cassette files for tools that were removed.
	deprecated := []string{"ls_tool", "multiedit_tool", "parallel_tool_calls"}
	for _, prov := range ucProviders() {
		for _, name := range deprecated {
			path := filepath.Join("testdata/TestCoderAgent", prov.dirName, name+".yaml")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Logf("Warning: could not delete %s: %v", path, err)
			} else if err == nil {
				t.Logf("Deleted deprecated cassette: %s", path)
			}
		}
	}

	for _, prov := range ucProviders() {
		t.Run(prov.dirName, func(t *testing.T) {
			for _, tc := range ucTestCases() {
				t.Run(tc.cassetteName, func(t *testing.T) {
					ucUpdateOneCassette(t, prov, tc)
				})
			}
		})
	}
}

// ucUpdateOneCassette handles the update for a single (provider, test) combination.
func ucUpdateOneCassette(t *testing.T, prov ucProviderSpec, tc ucTestCase) {
	t.Helper()

	cassettePath := filepath.Join("testdata/TestCoderAgent", prov.dirName, tc.cassetteName+".yaml")

	// ---- 1. Load and parse the old cassette ----
	raw, err := os.ReadFile(cassettePath)
	require.NoError(t, err, "reading old cassette %s", cassettePath)

	var oldCassette ucCassette
	require.NoError(t, yaml.Unmarshal(raw, &oldCassette), "parsing old cassette %s", cassettePath)

	// ---- 2. Filter: keep only 200-OK non-title-gen interactions ----
	var kept []*ucInteraction
	for _, i := range oldCassette.Interactions {
		if ucShouldKeep(i) {
			kept = append(kept, i)
		}
	}
	require.NotEmpty(t, kept, "no interactions kept after filtering %s", cassettePath)

	// ---- 3. Set up capturing transport that serves old responses in order ----
	ct := &capturingTransport{
		respond: func(n int, _ string) *http.Response {
			if n >= len(kept) {
				t.Fatalf("request index %d out of range (have %d filtered interactions)", n, len(kept))
			}
			// Build a fresh response with a new reader each time.
			return ucBuildHTTPResponse(kept[n].Response)
		},
	}
	httpClient := &http.Client{Transport: ct}

	// ---- 4. Create env with working dir matching TestCoderAgent ----
	// TestCoderAgent uses workingDir = "/tmp/crush-test/TestCoderAgent/{provider}/{test}"
	workingDir := filepath.Join("/tmp/crush-test/TestCoderAgent", prov.dirName, tc.cassetteName)
	env := testEnvWithDir(t, workingDir)
	createSimpleGoProject(t, workingDir)

	// ---- 5. Build agent (same settings as coderAgent in common_test.go) ----
	sa := buildCaptureAgent(t, env, httpClient, prov.makeProvider)

	// ---- 6. Run agent with the original prompt ----
	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, runErr := sa.Run(t.Context(), SessionAgentCall{
		Prompt:          tc.prompt,
		SessionID:       sess.ID,
		MaxOutputTokens: 10000,
		NonInteractive:  true,
	})
	if runErr != nil {
		// A run error is acceptable: the old responses may not produce a
		// perfectly coherent conversation. What matters is that requests
		// were captured before the error occurred.
		t.Logf("agent.Run returned (possibly expected) error: %v", runErr)
	}

	// ---- 7. Verify we captured exactly as many requests as filtered responses ----
	captured := ct.captured
	if len(captured) != len(kept) {
		t.Fatalf("captured %d requests but have %d filtered interactions (cassette: %s)",
			len(captured), len(kept), cassettePath)
	}

	// ---- 8. Build new cassette interactions ----
	newInteractions := make([]*ucInteraction, len(kept))
	for i, body := range captured {
		old := kept[i]
		newInteractions[i] = &ucInteraction{
			ID: i,
			Request: ucRequest{
				Proto:         old.Request.Proto,
				ProtoMajor:    old.Request.ProtoMajor,
				ProtoMinor:    old.Request.ProtoMinor,
				ContentLength: int64(len(body)),
				Host:          old.Request.Host,
				Body:          string(body),
				Headers:       old.Request.Headers,
				URL:           old.Request.URL,
				Method:        old.Request.Method,
			},
			Response: old.Response,
		}
	}

	newCassette := &ucCassette{
		Version:      2,
		Interactions: newInteractions,
	}

	// ---- 9. Marshal and write new cassette ----
	data, err := ucMarshal(newCassette)
	require.NoError(t, err)

	// Prepend YAML document separator (matches VCR library behaviour).
	out := append([]byte("---\n"), data...)
	require.NoError(t, os.WriteFile(cassettePath, out, 0o644))

	t.Logf("Updated %s (%d interactions)", cassettePath, len(newInteractions))
}
