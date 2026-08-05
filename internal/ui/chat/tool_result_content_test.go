package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

func TestHumanizedToolName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "snake case", input: "mcp_github_get", want: "Mcp Github Get"},
		{name: "kebab case", input: "web-fetch", want: "Web Fetch"},
		{name: "mixed", input: "job_output-tool", want: "Job Output Tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := humanizedToolName(tt.input); got != tt.want {
				t.Fatalf("humanizedToolName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLooksLikeMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "header", content: "# Title", want: true},
		{name: "code fence", content: "```go\nfmt.Println(\"x\")\n```", want: true},
		{name: "plain", content: "hello world", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeMarkdown(tt.content); got != tt.want {
				t.Fatalf("looksLikeMarkdown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolErrorContentQuietForModelInternalErrors(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	styPtr := &sty

	validationContent := "validation failed for tool \"read\":\n  - path.type: Value is null but should be string\n  - required: Required property 'path' is missing"
	jsonContent := "invalid JSON input: unexpected end of JSON input (raw: \"{\\\"path\\\":\")"

	t.Run("validation failure collapsed without ERROR banner", func(t *testing.T) {
		t.Parallel()
		got := toolErrorContent(styPtr, &message.ToolResult{Content: validationContent, IsError: true}, 80, false)
		if strings.Contains(got, "ERROR") {
			t.Fatalf("model-internal validation error must not render the ERROR banner, got %q", got)
		}
		if !strings.Contains(got, "validation failed for tool \"read\":") {
			t.Fatalf("collapsed message should keep the first line, got %q", got)
		}
	})

	t.Run("invalid JSON collapsed without ERROR banner", func(t *testing.T) {
		t.Parallel()
		got := toolErrorContent(styPtr, &message.ToolResult{Content: jsonContent, IsError: true}, 80, false)
		if strings.Contains(got, "ERROR") {
			t.Fatalf("model-internal JSON error must not render the ERROR banner, got %q", got)
		}
	})

	t.Run("expanded view keeps full message", func(t *testing.T) {
		t.Parallel()
		got := toolErrorContent(styPtr, &message.ToolResult{Content: validationContent, IsError: true}, 500, true)
		if !strings.Contains(got, "ERROR") {
			t.Fatalf("expanded view must keep the ERROR banner, got %q", got)
		}
		if !strings.Contains(got, "Required property 'path' is missing") {
			t.Fatalf("expanded view must include the full message, got %q", got)
		}
	})

	t.Run("user-actionable error keeps ERROR banner", func(t *testing.T) {
		t.Parallel()
		got := toolErrorContent(styPtr, &message.ToolResult{Content: "permission denied: /etc/passwd", IsError: true}, 80, false)
		if !strings.Contains(got, "ERROR") {
			t.Fatalf("user-actionable error must keep the ERROR banner, got %q", got)
		}
	})
}

func TestRenderToolResultTextContent(t *testing.T) {
	t.Parallel()

	sty := styles.DefaultStyles()
	styPtr := &sty
	widths := toolResultContentWidths{Body: 80, Diff: 82}

	t.Run("json branch", func(t *testing.T) {
		t.Parallel()
		content := `{"a":1}`
		var result json.RawMessage
		if err := json.Unmarshal([]byte(content), &result); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		prettyResult, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			t.Fatalf("json.MarshalIndent() error = %v", err)
		}
		expected := styPtr.Tool.Body.Render(toolOutputCodeContent(styPtr, "result.json", string(prettyResult), 0, widths.Body, false))
		got := renderToolResultTextContent(styPtr, content, widths, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose JSON rendering")
		}
	})

	t.Run("diff branch before markdown", func(t *testing.T) {
		t.Parallel()
		content := `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-# Old
+# New
`
		expected := toolOutputDiffContentFromUnified(styPtr, content, widths.Diff, false)
		got := renderToolResultTextContent(styPtr, content, widths, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose diff rendering")
		}
	})

	t.Run("markdown branch", func(t *testing.T) {
		t.Parallel()
		content := "# Title\n\nBody"
		expected := styPtr.Tool.Body.Render(toolOutputCodeContent(styPtr, "result.md", content, 0, widths.Body, false))
		got := renderToolResultTextContent(styPtr, content, widths, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose markdown rendering")
		}
	})

	t.Run("plain branch", func(t *testing.T) {
		t.Parallel()
		content := "plain text"
		expected := styPtr.Tool.Body.Render(toolOutputPlainContent(styPtr, content, widths.Body, false))
		got := renderToolResultTextContent(styPtr, content, widths, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose plain rendering")
		}
	})
}
