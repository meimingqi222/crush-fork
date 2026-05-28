package redact

import (
	"context"
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/plugin"
)

type redactPlugin struct {
	patterns []SecretPattern
}

func NewPlugin() plugin.Plugin {
	return &redactPlugin{}
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
			cache := make(map[string]string)

			for i := range output.Messages {
				msg := &output.Messages[i]
				redactMessageInfo(msg, p.patterns, cache)
				for j := range msg.Parts {
					part := &msg.Parts[j]
					redactPart(part, p.patterns, cache)
				}
			}

			return nil
		},
	}, nil
}

func (p *redactPlugin) Close(ctx context.Context) error {
	return nil
}

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
