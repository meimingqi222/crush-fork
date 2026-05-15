package agent

import (
	"context"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

func allowedToolNamesFromRuntimeContext(ctx context.Context) []string {
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(*sessionAgentRuntimeConfig); ok && runtimeConfig != nil {
		return normalizeToolNames(runtimeConfig.AllowedToolNames)
	}
	if runtimeConfig, ok := ctx.Value(sessionAgentRuntimeConfigContextKey{}).(sessionAgentRuntimeConfig); ok {
		return normalizeToolNames(runtimeConfig.AllowedToolNames)
	}
	return nil
}

func (c *coordinator) parentPermissionContext(ctx context.Context, mode session.CollaborationMode, sessionID string) (ParentPermissionContext, error) {
	allowedTools := allowedToolNamesFromRuntimeContext(ctx)
	if len(allowedTools) == 0 {
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
