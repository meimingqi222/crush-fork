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

	t.Run("missing thinking gets disabled", func(t *testing.T) {
		t.Parallel()
		out, ok := rewriteAnthropicBodyDisableThinking([]byte(`{"model":"x","messages":[]}`))
		if !ok {
			t.Fatalf("expected rewrite to occur")
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		thinking, ok := got["thinking"].(map[string]any)
		if !ok || thinking["type"] != "disabled" {
			t.Fatalf("expected type=disabled, got %v", got["thinking"])
		}
	})

	t.Run("explicit thinking is left untouched", func(t *testing.T) {
		t.Parallel()
		cases := []string{
			`{"model":"x","thinking":{"type":"enabled","budget_tokens":1024}}`,
			`{"model":"x","thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
			`{"model":"x","thinking":{"type":"disabled"}}`,
			`{"model":"x","thinking":null}`,
		}
		for _, in := range cases {
			if _, ok := rewriteAnthropicBodyDisableThinking([]byte(in)); ok {
				t.Fatalf("expected rewrite to be skipped for %s", in)
			}
		}
	})
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

	body := `{"model":"deepseek-v4-flash","messages":[]}`
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

func TestAnthropicDisableThinkingTransportPreservesEnabledThinking(t *testing.T) {
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

	body := `{"model":"x","thinking":{"type":"enabled","budget_tokens":1024}}`
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

	if string(receivedBody) != body {
		t.Fatalf("body should pass through untouched when thinking is set; got %q want %q", receivedBody, body)
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
