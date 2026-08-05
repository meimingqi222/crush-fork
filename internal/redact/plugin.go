package redact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
)

type redactPlugin struct {
	patterns []SecretPattern

	// persistentCache caches redaction results across ChatMessagesTransform
	// calls. The key is a SHA-256 hash of the input string; the value is the
	// redacted output. This avoids re-running 111 regex patterns on the same
	// text (e.g. old messages) on every LLM call, turning the transform from
	// O(N×M×111) per call into O(new_messages×M×111).
	//
	// The cache is guarded by cacheMu. It is evicted when it exceeds
	// maxPersistentCacheEntries to bound memory.
	cacheMu sync.Mutex
	cache   map[string]string
	cacheSz int
}

const (
	maxPersistentCacheEntries = 10_000
	// If a text is longer than this, skip caching it to bound memory.
	maxCachableTextLen = 512_000
)

func NewPlugin() plugin.Plugin {
	return &redactPlugin{
		cache: make(map[string]string),
	}
}

func (p *redactPlugin) Name() string {
	return "redact"
}

func (p *redactPlugin) Init(ctx context.Context, input plugin.PluginInput) (plugin.Hooks, error) {
	if cfg := input.Config.Config().Redact; cfg != nil && cfg.Enabled != nil && !*cfg.Enabled {
		slog.Info("redact plugin disabled via config")
		return plugin.Hooks{}, nil
	}

	p.patterns = make([]SecretPattern, len(BuiltinPatterns))
	copy(p.patterns, BuiltinPatterns)

	for i := range p.patterns {
		compilePattern(&p.patterns[i])
	}

	slog.Info("redact plugin enabled", "patterns", len(p.patterns))

	return plugin.Hooks{
		ToolBeforeExecute: func(ctx context.Context, input plugin.ToolBeforeExecuteInput) (*plugin.ToolBeforeExecuteOutput, error) {
			if input.Args == nil {
				return &plugin.ToolBeforeExecuteOutput{}, nil
			}
			cache := make(map[string]string)
			redacted := RedactDeep(input.Args, p.patterns, cache)
			if m, ok := redacted.(map[string]interface{}); ok {
				for k, v := range m {
					input.Args[k] = v
				}
			}
			return &plugin.ToolBeforeExecuteOutput{Args: input.Args}, nil
		},

		ToolAfterExecute: func(ctx context.Context, input plugin.ToolAfterExecuteInput) (*plugin.ToolAfterExecuteOutput, error) {
			cache := make(map[string]string)
			redacted := RedactString(input.Result, p.patterns, cache)
			changed := redacted != input.Result

			if input.Metadata != nil {
				redactedMeta := RedactDeep(input.Metadata, p.patterns, cache)
				if m, ok := redactedMeta.(map[string]interface{}); ok {
					return &plugin.ToolAfterExecuteOutput{
						Result:        redacted,
						ResultChanged: changed,
						Metadata:      m,
					}, nil
				}
			}

			return &plugin.ToolAfterExecuteOutput{
				Result:        redacted,
				ResultChanged: changed,
			}, nil
		},

		ChatMessagesTransform: func(ctx context.Context, input plugin.ChatMessagesTransformInput, output *plugin.ChatMessagesTransformOutput) error {
			// Use a per-call local cache seeded from the persistent cache.
			// The local cache avoids lock contention on every string lookup;
			// we only take the lock to get/set entries in the persistent cache.
			localCache := make(map[string]string, 64)

			for i := range output.Messages {
				msg := &output.Messages[i]
				p.redactMessageInfoCached(msg, localCache)
				for j := range msg.Parts {
					part := &msg.Parts[j]
					p.redactPartCached(part, localCache)
				}
			}

			return nil
		},
	}, nil
}

func (p *redactPlugin) Close(ctx context.Context) error {
	return nil
}

// redactStringCached looks up the persistent cache first, then falls back to
// running the full regex pipeline. On a miss, the result is stored in the
// persistent cache for future calls.
func (p *redactPlugin) redactStringCached(input string, localCache map[string]string) string {
	if input == "" {
		return input
	}

	// Fast path: check the per-call local cache.
	if cached, ok := localCache[input]; ok {
		return cached
	}

	// Check the persistent cache.
	key := hashKey(input)
	p.cacheMu.Lock()
	cached, ok := p.cache[key]
	p.cacheMu.Unlock()
	if ok {
		localCache[input] = cached
		return cached
	}

	// Miss: run the full redaction pipeline.
	result := RedactString(input, p.patterns, nil)
	localCache[input] = result

	// Store in the persistent cache if the text is not too large.
	if len(input) <= maxCachableTextLen {
		p.cacheMu.Lock()
		if p.cacheSz >= maxPersistentCacheEntries {
			// Evict all entries to bound memory (simple, infrequent).
			clear(p.cache)
			p.cacheSz = 0
		}
		if _, exists := p.cache[key]; !exists {
			p.cache[key] = result
			p.cacheSz++
		}
		p.cacheMu.Unlock()
	}

	return result
}

// redactToolInputCached is like redactStringCached but uses RedactToolInput.
// Its cache keys are prefixed with "t:" so results never collide with the
// string-redaction cache: RedactString and RedactToolInput can produce
// different output for the same input (JSON reformatting, image/base64 data
// handling), and sharing keys would return the wrong variant.
func (p *redactPlugin) redactToolInputCached(input string, localCache map[string]string) string {
	// Try JSON parse + deep redact (which may hit the cache for string values).
	// Fall back to plain string redaction.
	// We use the persistent cache for the final result.
	if input == "" {
		return input
	}

	// Check caches first.
	localKey := "t:" + input
	if cached, ok := localCache[localKey]; ok {
		return cached
	}
	key := "t:" + hashKey(input)
	p.cacheMu.Lock()
	cached, ok := p.cache[key]
	p.cacheMu.Unlock()
	if ok {
		localCache[localKey] = cached
		return cached
	}

	// Miss: run the full pipeline.
	result := RedactToolInput(input, p.patterns, nil)
	localCache[localKey] = result

	if len(input) <= maxCachableTextLen {
		p.cacheMu.Lock()
		if p.cacheSz >= maxPersistentCacheEntries {
			clear(p.cache)
			p.cacheSz = 0
		}
		if _, exists := p.cache[key]; !exists {
			p.cache[key] = result
			p.cacheSz++
		}
		p.cacheMu.Unlock()
	}

	return result
}

func (p *redactPlugin) redactMessageInfoCached(msg *message.Message, localCache map[string]string) {
	msg.Model = p.redactStringCached(msg.Model, localCache)
	msg.Provider = p.redactStringCached(msg.Provider, localCache)
	msg.SessionID = p.redactStringCached(msg.SessionID, localCache)
}

func (p *redactPlugin) redactPartCached(part *message.ContentPart, localCache map[string]string) {
	switch v := (*part).(type) {
	case message.TextContent:
		v.Text = p.redactStringCached(v.Text, localCache)
		*part = v
	case message.ReasoningContent:
		v.Thinking = p.redactStringCached(v.Thinking, localCache)
		*part = v
	case message.ToolCall:
		if v.Input != "" {
			v.Input = p.redactToolInputCached(v.Input, localCache)
		}
		*part = v
	case message.ToolResult:
		v.Content = p.redactStringCached(v.Content, localCache)
		v.Data = p.redactStringCached(v.Data, localCache)
		v.Metadata = p.redactStringCached(v.Metadata, localCache)
		*part = v
	}
}

// hashKey returns a hex-encoded SHA-256 hash of the input string.
func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// redactMessageInfo and redactPart are kept for backward compatibility
// (they are used by tests).
func redactMessageInfo(msg *message.Message, patterns []SecretPattern, cache map[string]string) {
	msg.Model = RedactString(msg.Model, patterns, cache)
	msg.Provider = RedactString(msg.Provider, patterns, cache)
	msg.SessionID = RedactString(msg.SessionID, patterns, cache)
}

func redactPart(part *message.ContentPart, patterns []SecretPattern, cache map[string]string) {
	switch v := (*part).(type) {
	case message.TextContent:
		v.Text = RedactString(v.Text, patterns, cache)
		*part = v
	case message.ReasoningContent:
		v.Thinking = RedactString(v.Thinking, patterns, cache)
		*part = v
	case message.ToolCall:
		if v.Input != "" {
			v.Input = RedactToolInput(v.Input, patterns, cache)
		}
		*part = v
	case message.ToolResult:
		v.Content = RedactString(v.Content, patterns, cache)
		v.Data = RedactString(v.Data, patterns, cache)
		v.Metadata = RedactString(v.Metadata, patterns, cache)
		*part = v
	}
}
