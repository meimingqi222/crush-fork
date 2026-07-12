package agent

import (
	"context"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

func allowedToolNamesFromRuntimeContext(ctx context.Context) ([]string, bool) {
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(*sessionAgentRuntimeConfig); ok && runtimeConfig != nil {
		return runtimeToolNames(runtimeConfig.Tools), true
	}
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(sessionAgentRuntimeConfig); ok {
		return runtimeToolNames(runtimeConfig.Tools), true
	}
	return nil, false
}

func runtimeToolNames(tools []fantasy.AgentTool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Info().Name)
	}
	return normalizeToolNames(names)
}

func (c *coordinator) parentPermissionContext(ctx context.Context, mode session.CollaborationMode, sessionID string) (ParentPermissionContext, error) {
	allowedTools, hasRuntimeConfig := allowedToolNamesFromRuntimeContext(ctx)
	if !hasRuntimeConfig {
		agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
		if ok {
			toolBuild, err := c.buildToolsWithContext(ctx, agentCfg, mode)
			if err != nil {
				return ParentPermissionContext{}, err
			}
			allowedTools = normalizeToolNames(toolBuild.RegisteredToolNames)
		}
	}
	return ParentPermissionContext{
		SessionID:    strings.TrimSpace(sessionID),
		AgentName:    config.AgentCoder,
		AllowedTools: allowedTools,
		ExternalDeny: append([]string(nil), c.cfg.Config().Options.DisabledTools...),
	}, nil
}
