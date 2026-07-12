package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/imageutil"
	"github.com/charmbracelet/crush/internal/permission"
)

// whitelistDockerTools contains Docker MCP tools that don't require permission.
var whitelistDockerTools = []string{
	"mcp_docker_mcp-find",
	"mcp_docker_mcp-add",
	"mcp_docker_mcp-remove",
	"mcp_docker_mcp-config-set",
	"mcp_docker_code-mode",
}

const mcpAdditionalMediaMetadataKey = "mcp_additional_media"

type mcpServerAccessContextKey struct{}

var scopedMCPServers sync.Map

// MCPServerAccess controls which process-global MCP connections are visible
// to the current execution. A missing access value preserves the normal Crush
// behavior where all configured MCP servers are available.
type MCPServerAccess interface {
	AllowsMCPServer(name string) bool
	MCPServerScope() string
	MCPServerRevision() uint64
}

// WithMCPServerAccess scopes MCP discovery and invocation to access.
func WithMCPServerAccess(ctx context.Context, access MCPServerAccess) context.Context {
	return context.WithValue(ctx, mcpServerAccessContextKey{}, access)
}

// MarkMCPServerScoped records that name is execution-scoped rather than a
// process-global static MCP server. The marker is intentionally permanent for
// the process lifetime so a removed dynamic server can never be mistaken for
// a newly discovered static server by an old cached tool object.
func MarkMCPServerScoped(name string) {
	if name != "" {
		scopedMCPServers.Store(name, struct{}{})
	}
}

// MCPServerAllowed reports whether name is available in the current scope.
func MCPServerAllowed(ctx context.Context, name string) bool {
	access, ok := ctx.Value(mcpServerAccessContextKey{}).(MCPServerAccess)
	if ok && access != nil {
		return access.AllowsMCPServer(name)
	}
	_, scoped := scopedMCPServers.Load(name)
	return !scoped
}

// MCPServerAccessRevision returns the current scope revision for cache keys.
func MCPServerAccessRevision(ctx context.Context) uint64 {
	access, ok := ctx.Value(mcpServerAccessContextKey{}).(MCPServerAccess)
	if !ok || access == nil {
		return 0
	}
	return access.MCPServerRevision()
}

// MCPServerAccessScope identifies the current access set for cache keys.
func MCPServerAccessScope(ctx context.Context) string {
	access, ok := ctx.Value(mcpServerAccessContextKey{}).(MCPServerAccess)
	if !ok || access == nil {
		return ""
	}
	return access.MCPServerScope()
}

type mcpAdditionalMediaMetadataItem struct {
	Type      string `json:"type"`
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
}

// GetMCPTools gets all the currently available MCP tools.
func GetMCPTools(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore, wd string) []*Tool {
	var result []*Tool
	for mcpName, tools := range mcp.Tools() {
		if !MCPServerAllowed(ctx, mcpName) {
			continue
		}
		for _, tool := range tools {
			result = append(result, &Tool{
				mcpName:     mcpName,
				tool:        tool,
				permissions: permissions,
				workingDir:  wd,
				cfg:         cfg,
			})
		}
	}
	return result
}

// Tool is a tool from a MCP.
type Tool struct {
	mcpName         string
	tool            *mcp.Tool
	cfg             *config.ConfigStore
	permissions     permission.Service
	workingDir      string
	providerOptions fantasy.ProviderOptions
}

func (m *Tool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.providerOptions = opts
}

func (m *Tool) ProviderOptions() fantasy.ProviderOptions {
	return m.providerOptions
}

func (m *Tool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", m.mcpName, m.tool.Name)
}

func (m *Tool) MCP() string {
	return m.mcpName
}

func (m *Tool) MCPToolName() string {
	return m.tool.Name
}

func (m *Tool) Info() fantasy.ToolInfo {
	parameters := make(map[string]any)
	required := make([]string, 0)

	if input, ok := m.tool.InputSchema.(map[string]any); ok {
		if props, ok := input["properties"].(map[string]any); ok {
			parameters = props
		}
		if req, ok := input["required"].([]any); ok {
			// Convert []any -> []string when elements are strings
			for _, v := range req {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		} else if reqStr, ok := input["required"].([]string); ok {
			// Handle case where it's already []string
			required = reqStr
		}
	}

	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

// normalizeMCPMediaPayload decodes the base64 MCP image payload, optionally
// compresses it, and returns the raw (unencoded) image bytes along with the
// (possibly updated) MIME type. Callers that need a base64 string must encode
// the returned bytes themselves.
func normalizeMCPMediaPayload(resultType string, data []byte, mimeType string, toolName string) ([]byte, string) {
	if len(data) == 0 {
		return data, mimeType
	}
	// MCP image and media payloads arrive base64-encoded in the Data field.
	if resultType != "image" && resultType != "media" {
		return data, mimeType
	}

	decoded, decodeErr := base64.StdEncoding.DecodeString(string(data))
	if decodeErr != nil {
		slog.Warn("Failed to decode base64 MCP image", "error", decodeErr, "tool", toolName)
		return data, mimeType
	}

	compressConfig := imageutil.DefaultCompressionConfig()
	compressResult, compressErr := imageutil.CompressImage(decoded, mimeType, compressConfig)
	if compressErr != nil {
		slog.Warn("Failed to compress MCP image", "error", compressErr, "tool", toolName)
		return decoded, mimeType
	}
	return compressResult.Data, compressResult.MimeType
}

func (m *Tool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if !MCPServerAllowed(ctx, m.mcpName) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("MCP server %q is not available in this session", m.mcpName)), nil
	}
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	// Skip permission for whitelisted Docker MCP tools.
	if !slices.Contains(whitelistDockerTools, m.Name()) {
		permissionDescription := fmt.Sprintf("execute %s with the following parameters:", m.Info().Name)
		permissionResponse, err := RequestPermission(ctx, m.permissions,
			permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  params.ID,
				Path:        m.workingDir,
				ToolName:    m.Info().Name,
				Action:      "execute",
				Description: permissionDescription,
				Params:      params.Input,
			},
		)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if permissionResponse != nil {
			return *permissionResponse, nil
		}
	}

	result, err := mcp.RunTool(ctx, m.cfg, m.mcpName, m.tool.Name, params.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	switch result.Type {
	case "image", "media":
		if !GetSupportsImagesFromContext(ctx) {
			vision := GetVisionServiceFromContext(ctx)
			if vision != nil && vision.IsAvailable() {
				rawImageData, mediaType := normalizeMCPMediaPayload(result.Type, result.Data, result.MediaType, m.tool.Name)
				desc, descErr := vision.DescribeImage(ctx, rawImageData, mediaType, "")
				if descErr != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to describe image from MCP tool: %v", descErr)), nil
				}
				return fantasy.NewTextResponse(desc), nil
			}
			modelName := GetModelNameFromContext(ctx)
			return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
		}

		rawImageData, mimeType := normalizeMCPMediaPayload(result.Type, result.Data, result.MediaType, m.tool.Name)

		var response fantasy.ToolResponse
		if result.Type == "image" {
			response = fantasy.NewImageResponse(rawImageData, mimeType)
		} else {
			response = fantasy.NewMediaResponse(rawImageData, mimeType)
		}
		response.Content = result.Content

		if len(result.AdditionalMedia) > 0 {
			additional := make([]mcpAdditionalMediaMetadataItem, 0, len(result.AdditionalMedia))
			for _, media := range result.AdditionalMedia {
				rawMediaData, mediaType := normalizeMCPMediaPayload(media.Type, media.Data, media.MediaType, m.tool.Name)
				additional = append(additional, mcpAdditionalMediaMetadataItem{
					Type:      media.Type,
					Data:      base64.StdEncoding.EncodeToString(rawMediaData),
					MediaType: mediaType,
				})
			}
			metadata, marshalErr := json.Marshal(map[string]any{
				mcpAdditionalMediaMetadataKey: additional,
			})
			if marshalErr != nil {
				slog.Warn("Failed to encode MCP additional media metadata", "error", marshalErr, "tool", m.tool.Name)
			} else {
				response.Metadata = string(metadata)
			}
		}

		return response, nil
	default:
		return fantasy.NewTextResponse(result.Content), nil
	}
}
