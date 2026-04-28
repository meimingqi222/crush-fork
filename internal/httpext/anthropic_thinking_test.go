package httpext

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriteAnthropicBodyDisableThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
	}{
		{
			name: "no thinking field",
			in:   `{"model":"x","messages":[]}`,
		},
		{
			name: "thinking already enabled",
			in:   `{"model":"x","thinking":{"type":"enabled","budget_tokens":1024}}`,
		},
		{
			name: "thinking adaptive",
			in:   `{"model":"x","thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, ok := rewriteAnthropicBodyDisableThinking([]byte(tc.in))
			if !ok {
				t.Fatalf("expected rewrite to succeed")
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			thinking, ok := got["thinking"].(map[string]any)
			if !ok {
				t.Fatalf("thinking not an object: %#v", got["thinking"])
			}
			if thinking["type"] != "disabled" {
				t.Fatalf("expected type=disabled, got %v", thinking["type"])
			}
			if _, hasBudget := thinking["budget_tokens"]; hasBudget {
				t.Fatalf("budget_tokens should be cleared, got %v", thinking["budget_tokens"])
			}
		})
	}
}

func TestRewriteAnthropicBodyDisableThinkingNonObject(t *testing.T) {
	t.Parallel()
	if _, ok := rewriteAnthropicBodyDisableThinking([]byte(`[1,2,3]`)); ok {
		t.Fatalf("expected rewrite to be skipped for non-object body")
	}
	if _, ok := rewriteAnthropicBodyDisableThinking([]byte(``)); ok {
		t.Fatalf("expected rewrite to be skipped for empty body")
	}
	if _, ok := rewriteAnthropicBodyDisableThinking([]byte(`not json`)); ok {
		t.Fatalf("expected rewrite to be skipped for invalid JSON")
	}
}

func TestAnthropicDisableThinkingTransport(t *testing.T) {
	t.Parallel()

	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := WrapAnthropicDisableThinkingHTTPClient(srv.Client())

	body := `{"model":"deepseek-v4-flash","thinking":{"type":"enabled","budget_tokens":1024}}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var got map[string]any
	if err := json.Unmarshal(receivedBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	thinking, ok := got["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("expected forwarded body to set thinking.type=disabled, got %s", string(receivedBody))
	}
}

func TestAnthropicDisableThinkingTransportSkipsNonMessages(t *testing.T) {
	t.Parallel()

	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := WrapAnthropicDisableThinkingHTTPClient(srv.Client())

	body := `{"model":"x"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/models", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if string(receivedBody) != body {
		t.Fatalf("body should be untouched for non-/messages endpoint; got %q want %q", string(receivedBody), body)
	}
}
