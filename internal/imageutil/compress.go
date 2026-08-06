package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"

	"github.com/disintegration/imaging"
	// Register WebP decoder so imaging.Decode can handle image/webp.
	_ "golang.org/x/image/webp"
)

// CompressionConfig holds the configuration for image compression.
type CompressionConfig struct {
	// MaxSizeBytes is the maximum size in bytes before compression is triggered.
	// Default is 1MB.
	MaxSizeBytes int64

	// JPEGQuality is the quality for JPEG compression (1-100).
	// Default is 75 which provides good quality with smaller size.
	JPEGQuality int

	// MaxDimension is the maximum width or height for the image.
	// Images larger than this will be resized proportionally.
	// Default is 2000 pixels, matching the OpenAI vision model input limit.
	MaxDimension int
}

// DefaultCompressionConfig returns the default compression configuration.
func DefaultCompressionConfig() CompressionConfig {
	return CompressionConfig{
		MaxSizeBytes: 1024 * 1024, // 1MB
		JPEGQuality:  75,
		MaxDimension: 2000,
	}
}

// CompressResult contains the compressed image data and metadata.
type CompressResult struct {
	Data           []byte
	MimeType       string
	WasCompressed  bool
	OriginalSize   int64
	CompressedSize int64
}

// DetectMimeType detects the MIME type from image data.
func DetectMimeType(data []byte) string {
	if len(data) < 3 {
		return ""
	}

	// Check for PNG.
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}

	// Check for JPEG.
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}

	// Check for GIF.
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 {
		return "image/gif"
	}

	// Check for WebP.
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}

	return ""
}

// ShouldCompress checks if the image data should be compressed based on size.
// Note: dimension-based compression is checked inside CompressImage after
// decoding, since dimensions cannot be determined from raw bytes alone.
func ShouldCompress(data []byte, config CompressionConfig) bool {
	return int64(len(data)) > config.MaxSizeBytes
}

// CompressImage compresses the image if it exceeds the size threshold or if
// its dimensions exceed MaxDimension. It converts PNG/GIF/WebP to JPEG for
// better compression when the image exceeds the size threshold. For images
// with transparency, it preserves PNG format.
//
// If the image dimensions exceed MaxDimension, the image is always resized
// (even if the re-encoded result is larger than the original), because
// oversized images are rejected by some vision model APIs (e.g. OpenAI's
// 2000px limit).
func CompressImage(data []byte, mimeType string, config CompressionConfig) (*CompressResult, error) {
	originalSize := int64(len(data))

	// Fast path: if under the size threshold, we still need to check
	// dimensions. Decode the image to inspect its size, and only resize
	// if it exceeds MaxDimension.
	sizeOver := ShouldCompress(data, config)

	if !sizeOver {
		// Size is under threshold — check dimensions by decoding.
		img, dimErr := decodeImage(data, mimeType)
		if dimErr != nil {
			// Can't decode (e.g. SVG or corrupted); return as-is.
			slog.Debug("Could not decode image for dimension check, returning original",
				"error", dimErr, "mime_type", mimeType)
			return &CompressResult{
				Data:           data,
				MimeType:       mimeType,
				WasCompressed:  false,
				OriginalSize:   originalSize,
				CompressedSize: originalSize,
			}, nil
		}

		bounds := img.Bounds()
		if bounds.Dx() <= config.MaxDimension && bounds.Dy() <= config.MaxDimension {
			// Both size and dimensions are within limits — no work needed.
			return &CompressResult{
				Data:           data,
				MimeType:       mimeType,
				WasCompressed:  false,
				OriginalSize:   originalSize,
				CompressedSize: originalSize,
			}, nil
		}

		// Dimensions exceed the limit — must resize even though file
		// size is small. Fall through to the resize + re-encode path.
		return resizeAndEncode(img, data, mimeType, originalSize, config)
	}

	// Size exceeds threshold — decode, resize, and re-encode.
	img, err := decodeImage(data, mimeType)
	if err != nil {
		slog.Warn("Failed to decode image for compression, returning original", "error", err, "mime_type", mimeType)
		return &CompressResult{
			Data:           data,
			MimeType:       mimeType,
			WasCompressed:  false,
			OriginalSize:   originalSize,
			CompressedSize: originalSize,
		}, nil
	}

	return resizeAndEncode(img, data, mimeType, originalSize, config)
}

// decodeImage decodes image data using the appropriate decoder based on
// mimeType. Falls back to generic imaging.Decode for unsupported types.
func decodeImage(data []byte, mimeType string) (image.Image, error) {
	reader := bytes.NewReader(data)
	switch mimeType {
	case "image/jpeg":
		return jpeg.Decode(reader)
	case "image/png":
		return png.Decode(reader)
	default:
		return imaging.Decode(reader)
	}
}

// resizeAndEncode resizes the image if needed, then re-encodes it. When the
// resize was triggered by dimension overflow, the result is always used even
// if it is larger than the original, because oversized images are rejected by
// vision model APIs. When triggered only by file size, the result is used
// only if it is actually smaller.
func resizeAndEncode(img image.Image, originalData []byte, originalMimeType string, originalSize int64, config CompressionConfig) (*CompressResult, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dimensionOver := width > config.MaxDimension || height > config.MaxDimension

	if dimensionOver {
		img = imaging.Fit(img, config.MaxDimension, config.MaxDimension, imaging.Lanczos)
	}

	// Check if image has transparency.
	hasTransparency := hasAlpha(img)

	var output bytes.Buffer
	var outputMimeType string

	if hasTransparency {
		outputMimeType = "image/png"
		if err := png.Encode(&output, img); err != nil {
			slog.Warn("Failed to encode PNG, returning original", "error", err)
			return &CompressResult{
				Data:           originalData,
				MimeType:       originalMimeType,
				WasCompressed:  false,
				OriginalSize:   originalSize,
				CompressedSize: originalSize,
			}, nil
		}
	} else {
		outputMimeType = "image/jpeg"
		if err := jpeg.Encode(&output, img, &jpeg.Options{Quality: config.JPEGQuality}); err != nil {
			slog.Warn("Failed to encode JPEG, returning original", "error", err)
			return &CompressResult{
				Data:           originalData,
				MimeType:       originalMimeType,
				WasCompressed:  false,
				OriginalSize:   originalSize,
				CompressedSize: originalSize,
			}, nil
		}
	}

	compressedData := output.Bytes()
	compressedSize := int64(len(compressedData))

	// If the resize was triggered by dimension overflow, always use the
	// re-encoded result even if it is larger — the API would reject the
	// oversized original anyway.
	if !dimensionOver && compressedSize >= originalSize {
		slog.Debug("Compression did not reduce size, keeping original",
			"original_size", originalSize,
			"compressed_size", compressedSize,
		)
		return &CompressResult{
			Data:           originalData,
			MimeType:       originalMimeType,
			WasCompressed:  false,
			OriginalSize:   originalSize,
			CompressedSize: originalSize,
		}, nil
	}

	slog.Debug("Image compressed",
		"original_size", originalSize,
		"compressed_size", compressedSize,
		"ratio", float64(compressedSize)/float64(originalSize),
		"output_format", outputMimeType,
		"dimension_resized", dimensionOver,
	)

	return &CompressResult{
		Data:           compressedData,
		MimeType:       outputMimeType,
		WasCompressed:  true,
		OriginalSize:   originalSize,
		CompressedSize: compressedSize,
	}, nil
}

// hasAlpha checks if the image has an alpha channel with non-opaque pixels.
func hasAlpha(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a < 0xFFFF {
				return true
			}
		}
	}
	return false
}

// CompressFromReader reads image data from a reader and compresses it if needed.
func CompressFromReader(r io.Reader, mimeType string, config CompressionConfig) (*CompressResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	// Auto-detect MIME type if not provided.
	if mimeType == "" {
		mimeType = DetectMimeType(data)
	}

	return CompressImage(data, mimeType, config)
}
