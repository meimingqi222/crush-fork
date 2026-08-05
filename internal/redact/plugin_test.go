package redact

import (
	"context"
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
	"github.com/stretchr/testify/require"
)

// newTestRedactPlugin creates a redactPlugin with patterns pre-compiled,
// bypassing Init (which needs a ConfigStore).
func newTestRedactPlugin() *redactPlugin {
	rp := &redactPlugin{
		patterns: make([]SecretPattern, len(BuiltinPatterns)),
		cache:    make(map[string]string),
	}
	copy(rp.patterns, BuiltinPatterns)
	for i := range rp.patterns {
		compilePattern(&rp.patterns[i])
	}
	return rp
}

// TestRedactPluginChatMessagesTransformCaching verifies that the persistent
// cache correctly avoids re-redacting the same text on subsequent calls.
func TestRedactPluginChatMessagesTransformCaching(t *testing.T) {
	rp := newTestRedactPlugin()

	// Build hooks manually (skip Init which needs ConfigStore).
	hooks := plugin.Hooks{
		ChatMessagesTransform: func(ctx context.Context, input plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			localCache := make(map[string]string, 64)
			for i := range output.Messages {
				msg := &output.Messages[i]
				rp.redactMessageInfoCached(msg, localCache)
				for j := range msg.Parts {
					part := &msg.Parts[j]
					rp.redactPartCached(part, localCache)
				}
			}
			return nil
		},
	}

	// Messages with a fake API key in the text.
	msgs := []message.Message{
		{ID: "msg-1", Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "Here is my key: ghp_123456789012345678901234567890123456"},
		}},
		{ID: "msg-2", Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "I received your key, thanks."},
		}},
	}

	// First call: should redact the key.
	out1 := plugin.ChatMessagesTransformOutput{Messages: cloneMsgs(msgs)}
	err := hooks.ChatMessagesTransform(context.Background(), plugin.ChatMessagesTransformInput{}, &out1)
	require.NoError(t, err)

	text1 := out1.Messages[0].Parts[0].(message.TextContent).Text
	require.Contains(t, text1, "[REDACTED:github-pat]")
	require.NotContains(t, text1, "ghp_123456789012345678901234567890123456")

	// Second call with the SAME messages: should use cache, produce same result.
	out2 := plugin.ChatMessagesTransformOutput{Messages: cloneMsgs(msgs)}
	err = hooks.ChatMessagesTransform(context.Background(), plugin.ChatMessagesTransformInput{}, &out2)
	require.NoError(t, err)

	text2 := out2.Messages[0].Parts[0].(message.TextContent).Text
	require.Equal(t, text1, text2)

	// Cache should have entries.
	rp.cacheMu.Lock()
	cacheSz := rp.cacheSz
	rp.cacheMu.Unlock()
	require.Greater(t, cacheSz, 0, "persistent cache should have entries after first call")
}

// TestRedactPluginCacheMissOnNewText verifies that new text is redacted correctly.
func TestRedactPluginCacheMissOnNewText(t *testing.T) {
	rp := newTestRedactPlugin()

	hooks := plugin.Hooks{
		ChatMessagesTransform: func(ctx context.Context, input plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			localCache := make(map[string]string, 64)
			for i := range output.Messages {
				msg := &output.Messages[i]
				rp.redactMessageInfoCached(msg, localCache)
				for j := range msg.Parts {
					part := &msg.Parts[j]
					rp.redactPartCached(part, localCache)
				}
			}
			return nil
		},
	}

	// First message with one key.
	msgs1 := []message.Message{
		{ID: "msg-1", Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "key1: ghp_123456789012345678901234567890123456"},
		}},
	}
	out1 := plugin.ChatMessagesTransformOutput{Messages: cloneMsgs(msgs1)}
	_ = hooks.ChatMessagesTransform(context.Background(), plugin.ChatMessagesTransformInput{}, &out1)
	require.Contains(t, out1.Messages[0].Parts[0].(message.TextContent).Text, "[REDACTED:github-pat]")

	// Second message with a DIFFERENT key.
	msgs2 := []message.Message{
		{ID: "msg-2", Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "key2: ghp_987654321098765432109876543210987654321"},
		}},
	}
	out2 := plugin.ChatMessagesTransformOutput{Messages: cloneMsgs(msgs2)}
	_ = hooks.ChatMessagesTransform(context.Background(), plugin.ChatMessagesTransformInput{}, &out2)
	require.Contains(t, out2.Messages[0].Parts[0].(message.TextContent).Text, "[REDACTED:github-pat]")
}

// BenchmarkRedactPluginChatMessagesTransform_N85_Cached simulates 85 messages
// being transformed on each call, measuring the improvement from caching.
// After the first iteration, all messages should be cache hits.
func BenchmarkRedactPluginChatMessagesTransform_N85_Cached(b *testing.B) {
	rp := newTestRedactPlugin()

	hooks := plugin.Hooks{
		ChatMessagesTransform: func(ctx context.Context, input plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			localCache := make(map[string]string, 64)
			for i := range output.Messages {
				msg := &output.Messages[i]
				rp.redactMessageInfoCached(msg, localCache)
				for j := range msg.Parts {
					part := &msg.Parts[j]
					rp.redactPartCached(part, localCache)
				}
			}
			return nil
		},
	}

	// Create 85 messages, some with secrets.
	msgs := make([]message.Message, 85)
	for i := range msgs {
		text := fmt.Sprintf("This is message number %d with some text", i)
		if i%10 == 0 {
			text += " ghp_123456789012345678901234567890123456"
		}
		msgs[i] = message.Message{
			ID:   fmt.Sprintf("msg-%d", i),
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: text},
			},
		}
	}

	b.ResetTimer()
	for b.Loop() {
		out := plugin.ChatMessagesTransformOutput{Messages: cloneMsgs(msgs)}
		if err := hooks.ChatMessagesTransform(context.Background(), plugin.ChatMessagesTransformInput{}, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRedactPluginChatMessagesTransform_N85_NoCache benchmarks the old
// per-call cache behavior (no persistent cache) for comparison.
func BenchmarkRedactPluginChatMessagesTransform_N85_NoCache(b *testing.B) {
	rp := newTestRedactPlugin()

	msgs := make([]message.Message, 85)
	for i := range msgs {
		text := fmt.Sprintf("This is message number %d with some text", i)
		if i%10 == 0 {
			text += " ghp_123456789012345678901234567890123456"
		}
		msgs[i] = message.Message{
			ID:   fmt.Sprintf("msg-%d", i),
			Role: message.User,
			Parts: []message.ContentPart{
				message.TextContent{Text: text},
			},
		}
	}

	b.ResetTimer()
	for b.Loop() {
		// Simulate the old behavior: per-call cache only.
		cache := make(map[string]string)
		for i := range msgs {
			msg := msgs[i] // copy
			redactMessageInfo(&msg, rp.patterns, cache)
			for j := range msg.Parts {
				redactPart(&msg.Parts[j], rp.patterns, cache)
			}
		}
	}
}

func cloneMsgs(msgs []message.Message) []message.Message {
	cloned := make([]message.Message, len(msgs))
	for i := range msgs {
		cloned[i] = msgs[i].Clone()
	}
	return cloned
}

// TestRedactPluginCacheKindIsolation verifies that the string-redaction and
// tool-input-redaction caches never share entries: RedactString and
// RedactToolInput can produce different output for the same input (JSON
// reformatting, image/base64 data handling), so a shared key space would
// return the wrong variant and leak unredacted data.
func TestRedactPluginCacheKindIsolation(t *testing.T) {
	rp := &redactPlugin{
		patterns: []SecretPattern{
			{ID: "test-secret", Pattern: `(SECRET-[A-Z0-9]+)`, Keywords: []string{"SECRET-"}},
		},
		cache: make(map[string]string),
	}
	for i := range rp.patterns {
		compilePattern(&rp.patterns[i])
	}

	// JSON with image data: RedactToolInput skips the data field (deep
	// redact), while RedactString replaces the secret.
	input := `{"type": "image", "data": "SECRET-123", "note": "keep"}`

	wantTool := RedactToolInput(input, rp.patterns, nil)
	wantString := RedactString(input, rp.patterns, nil)
	require.NotEqual(t, wantTool, wantString, "test requires different outputs per kind")
	require.NotContains(t, wantTool, "[REDACTED:test-secret]")
	require.Contains(t, wantString, "[REDACTED:test-secret]")

	// Tool path first, then string path: the string result must NOT come
	// from the tool cache.
	localCache := make(map[string]string)
	toolResult := rp.redactToolInputCached(input, localCache)
	require.Equal(t, wantTool, toolResult)

	stringResult := rp.redactStringCached(input, localCache)
	require.Equal(t, wantString, stringResult, "string path must not hit the tool-input cache entry")

	// Same call order across transform invocations (fresh local cache,
	// persistent cache persists).
	localCache2 := make(map[string]string)
	require.Equal(t, wantTool, rp.redactToolInputCached(input, localCache2))
	require.Equal(t, wantString, rp.redactStringCached(input, localCache2))

	// Reverse order: string first, then tool.
	localCache3 := make(map[string]string)
	require.Equal(t, wantString, rp.redactStringCached(input, localCache3))
	require.Equal(t, wantTool, rp.redactToolInputCached(input, localCache3), "tool path must not hit the string cache entry")
}

// TestRedactPluginToolBeforeExecuteIsNoop verifies that the redact plugin
// never alters the arguments a tool actually executes with. Redacting
// execution args would corrupt tool behavior (e.g. a write tool receiving
// [REDACTED] content) and would diverge the stored ToolCall.Input from what
// actually ran.
func TestRedactPluginToolBeforeExecuteIsNoop(t *testing.T) {
	store, err := config.Init(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	// config.Init sets up a lumberjack log file that holds the file open;
	// release it so t.TempDir() cleanup can remove the directory on Windows.
	t.Cleanup(func() { _ = log.ResetForTesting() })

	hooks, err := NewPlugin().Init(context.Background(), plugin.PluginInput{Config: store})
	require.NoError(t, err)
	require.NotNil(t, hooks.ToolBeforeExecute)

	out, err := hooks.ToolBeforeExecute(context.Background(), plugin.ToolBeforeExecuteInput{
		Tool:   "write",
		CallID: "call-1",
		Args: map[string]any{
			"file":    "/tmp/x.txt",
			"content": "token [REDACTED:sk-secret] value",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Nil(t, out.Args, "redact must not alter execution args")
	require.False(t, out.Skip)
}
