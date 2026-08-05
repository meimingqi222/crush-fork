package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
)

const DescribeImageToolName = "describe_image"

//go:embed describe_image.md
var describeImageDescription []byte

type DescribeImageParams struct {
	Path       string `json:"path,omitempty" description:"Path to the image file to describe (e.g. screenshot.png)"`
	MessageID  string `json:"message_id,omitempty" description:"Message ID that contains the image attachment to describe"`
	ImageIndex int    `json:"image_index,omitempty" description:"1-based index of the image attachment within the message"`
	Prompt     string `json:"prompt,omitempty" description:"Optional instruction for what to focus on in the description"`
}

type DescribeImagePermissionsParams struct {
	Path       string `json:"path,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	ImageIndex int    `json:"image_index,omitempty"`
}

type DescribeImageResponseMetadata struct {
	ToolPathMetadata
	Path       string `json:"path,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	ImageIndex int    `json:"image_index,omitempty"`
}

// NewDescribeImageTool creates a tool that uses a vision-capable helper model
// to describe image content. It is only registered when the primary model does
// not support images and a vision helper model is configured.
func NewDescribeImageTool(permissions permission.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		DescribeImageToolName,
		string(describeImageDescription),
		func(ctx context.Context, params DescribeImageParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Path == "" && params.MessageID == "" {
				return fantasy.NewTextErrorResponse("path or message_id is required"), nil
			}

			vision := GetVisionServiceFromContext(ctx)
			if vision == nil || !vision.IsAvailable() {
				return fantasy.NewTextErrorResponse("No vision helper model is configured. Set a \"vision\" model in crush.json to enable image descriptions."), nil
			}

			effectiveWorkingDir := EffectiveWorkingDir(ctx, workingDir)
			var toolPath ToolPath
			if params.Path != "" && params.MessageID == "" {
				toolPath = ResolveToolPath(ctx, effectiveWorkingDir, params.Path)
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required")
			}

			filePath := params.Path
			if params.MessageID == "" {
				filePath = toolPath.AbsolutePath

				absWorkingDir, err := filepath.Abs(effectiveWorkingDir)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error resolving working directory: %w", err)
				}
				absFilePath, err := filepath.Abs(filePath)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error resolving file path: %w", err)
				}
				relPath, relErr := filepath.Rel(absWorkingDir, absFilePath)
				isOutsideWorkDir := relErr != nil || strings.HasPrefix(relPath, "..")
				if isOutsideWorkDir {
					permissionResponse, permErr := RequestPermission(ctx, permissions,
						permission.CreatePermissionRequest{
							SessionID:   sessionID,
							Path:        absFilePath,
							ToolCallID:  call.ID,
							ToolName:    DescribeImageToolName,
							Action:      "read",
							Description: fmt.Sprintf("Describe image outside working directory: %s", absFilePath),
							Params: DescribeImagePermissionsParams{
								Path:       params.Path,
								MessageID:  params.MessageID,
								ImageIndex: params.ImageIndex,
							},
						},
					)
					if permErr != nil {
						return fantasy.ToolResponse{}, permErr
					}
					if permissionResponse != nil {
						return *permissionResponse, nil
					}
				}
			}

			image, readErr := loadImageDataForDescribe(ctx, DescribeImageParams{
				Path:       filePath,
				MessageID:  params.MessageID,
				ImageIndex: params.ImageIndex,
			})
			if readErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Could not read image: %v", readErr)), nil
			}
			if !isSupportedImageMimeType(image.mimeType) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Unsupported image format: %s. Supported formats: jpg, jpeg, png, gif, webp.", image.mimeType)), nil
			}

			description, descErr := vision.DescribeImage(ctx, image.data, image.mimeType, params.Prompt)
			if descErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to describe image: %v", descErr)), nil
			}

			meta := DescribeImageResponseMetadata{
				Path:       image.path,
				MessageID:  image.messageID,
				ImageIndex: image.imageIndex,
			}
			if toolPath.AbsolutePath != "" {
				meta.ToolPathMetadata = NewToolPathMetadata(toolPath)
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(description),
				meta,
			), nil
		},
	)
}

type describeImageData struct {
	data       []byte
	mimeType   string
	path       string
	messageID  string
	imageIndex int
}

// loadImageDataForDescribe reads image bytes and MIME type from disk when
// possible. If the file does not exist, it falls back to the session message
// history so the LLM can describe images that were attached to the current
// conversation rather than only files on disk.
func loadImageDataForDescribe(ctx context.Context, params DescribeImageParams) (describeImageData, error) {
	if params.MessageID != "" || params.ImageIndex > 0 {
		return loadImageDataForDescribeFromMessage(ctx, params.MessageID, params.ImageIndex)
	}

	filePath := params.Path
	imageData, err := os.ReadFile(filePath)
	if err == nil {
		_, mimeType := getImageMimeType(filePath)
		return describeImageData{data: imageData, mimeType: mimeType, path: filePath}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return describeImageData{}, err
	}

	sessionID := GetSessionFromContext(ctx)
	messageSvc := GetMessageServiceFromContext(ctx)
	if sessionID == "" || messageSvc == nil {
		return describeImageData{}, err
	}

	msgs, listErr := messageSvc.List(ctx, sessionID)
	if listErr != nil {
		return describeImageData{}, fmt.Errorf("%w (and failed to list session messages: %v)", err, listErr)
	}

	target := filepath.Base(filePath)
	matches := make([]describeImageData, 0, 1)
	for _, msg := range msgs {
		imageIndex := 0
		for _, part := range msg.Parts {
			bc, ok := part.(message.BinaryContent)
			if !ok {
				continue
			}
			if !strings.HasPrefix(bc.MIMEType, "image/") {
				continue
			}
			imageIndex++
			name := filepath.Base(bc.Path)
			if name == target || bc.Path == filePath {
				matches = append(matches, describeImageData{
					data:       bc.Data,
					mimeType:   bc.MIMEType,
					path:       bc.Path,
					messageID:  msg.ID,
					imageIndex: imageIndex,
				})
			}
		}
	}
	switch len(matches) {
	case 0:
		return describeImageData{}, err
	case 1:
		return matches[0], nil
	default:
		return describeImageData{}, fmt.Errorf("image path %q is ambiguous; %d matching image attachments were found. Use message_id and image_index from the attachment placeholder", params.Path, len(matches))
	}
}

func loadImageDataForDescribeFromMessage(ctx context.Context, messageID string, imageIndex int) (describeImageData, error) {
	if messageID == "" {
		return describeImageData{}, errors.New("message_id is required when image_index is provided")
	}
	if imageIndex <= 0 {
		return describeImageData{}, errors.New("image_index must be greater than 0 when message_id is provided")
	}

	sessionID := GetSessionFromContext(ctx)
	messageSvc := GetMessageServiceFromContext(ctx)
	if sessionID == "" {
		return describeImageData{}, errors.New("session ID is required")
	}
	if messageSvc == nil {
		return describeImageData{}, errors.New("message service is required")
	}

	msgs, err := messageSvc.List(ctx, sessionID)
	if err != nil {
		return describeImageData{}, fmt.Errorf("failed to list session messages: %w", err)
	}
	for _, msg := range msgs {
		if msg.ID != messageID {
			continue
		}
		currentImage := 0
		for _, part := range msg.Parts {
			bc, ok := part.(message.BinaryContent)
			if !ok || !strings.HasPrefix(bc.MIMEType, "image/") {
				continue
			}
			currentImage++
			if currentImage == imageIndex {
				return describeImageData{
					data:       bc.Data,
					mimeType:   bc.MIMEType,
					path:       bc.Path,
					messageID:  msg.ID,
					imageIndex: imageIndex,
				}, nil
			}
		}
		return describeImageData{}, fmt.Errorf("message %q has no image attachment at image_index %d", messageID, imageIndex)
	}
	return describeImageData{}, fmt.Errorf("message %q was not found in session %q", messageID, sessionID)
}

// isSupportedImageMimeType reports whether mimeType is one of the image
// formats the vision helper can process.
func isSupportedImageMimeType(mimeType string) bool {
	switch strings.ToLower(mimeType) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}
