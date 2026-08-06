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
	// MaxSizeBytes is the target maximum size in bytes. Images exceeding this
	// are compressed through a quality-then-dimension ladder. Default is 1MB.
	MaxSizeBytes int64

	// JPEGQuality is the initial JPEG quality (1-100). If the result still
	// exceeds MaxSizeBytes, lower qualities are tried (60, 45, 30). Default
	// is 75 which provides good quality with smaller size.
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
// its dimensions exceed MaxDimension.
//
// Compression strategy (multi-step ladder):
//  1. Encode at the initial JPEG quality, trying both PNG and JPEG — pick the
//     smaller. For images with transparency only PNG is produced (JPEG can't
//     represent alpha).
//  2. If still over MaxSizeBytes, walk a JPEG quality ladder (60 → 45 → 30).
//  3. If still over MaxSizeBytes, progressively scale down dimensions
//     (75% → 50% → 35%) and re-run the quality ladder at each scale.
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
		// Size is under threshold - check dimensions by decoding.
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
			// Both size and dimensions are within limits - no work needed.
			return &CompressResult{
				Data:           data,
				MimeType:       mimeType,
				WasCompressed:  false,
				OriginalSize:   originalSize,
				CompressedSize: originalSize,
			}, nil
		}

		// Dimensions exceed the limit - must resize even though file
		// size is small. Fall through to the resize + re-encode path.
		return resizeAndEncode(img, data, mimeType, originalSize, config)
	}

	// Size exceeds threshold - decode, resize, and re-encode.
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

// qualitySteps are the JPEG quality fallbacks tried (in order) when the
// initial encoding exceeds MaxSizeBytes.
var qualitySteps = []int{60, 45, 30}

// scaleSteps are the dimension scale factors tried (in order) when the
// quality ladder alone cannot bring the image under MaxSizeBytes.
var scaleSteps = []float64{0.75, 0.5, 0.35}

// encodedImage holds encoded bytes and their MIME type.
type encodedImage struct {
	data     []byte
	mimeType string
}

// encodePNG encodes an image as PNG. Returns nil on encoding failure
// (should not happen with bytes.Buffer).
func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		slog.Warn("Failed to encode PNG", "error", err)
		return nil
	}
	return buf.Bytes()
}

// encodeJPEG encodes an image as JPEG at the given quality. Returns nil on
// encoding failure (should not happen with bytes.Buffer).
func encodeJPEG(img image.Image, quality int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		slog.Warn("Failed to encode JPEG", "error", err)
		return nil
	}
	return buf.Bytes()
}

// encodeWithLadder encodes img trying both PNG and JPEG (at multiple quality
// levels), returning the smallest result. For transparent images only PNG is
// produced (JPEG cannot represent alpha). The quality ladder starts at
// config.JPEGQuality and descends through qualitySteps when the result still
// exceeds config.MaxSizeBytes.
func encodeWithLadder(img image.Image, hasTransparency bool, config CompressionConfig) encodedImage {
	if hasTransparency {
		return encodedImage{data: encodePNG(img), mimeType: "image/png"}
	}

	// Encode PNG once (quality-independent).
	pngData := encodePNG(img)

	// Try JPEG at initial quality, pick the smaller of the two.
	best := encodedImage{data: encodeJPEG(img, config.JPEGQuality), mimeType: "image/jpeg"}
	if pngData != nil && (best.data == nil || len(pngData) < len(best.data)) {
		best = encodedImage{data: pngData, mimeType: "image/png"}
	}

	// Quality ladder: try lower JPEG qualities if still over budget.
	if best.data != nil && int64(len(best.data)) > config.MaxSizeBytes {
		for _, q := range qualitySteps {
			candidate := encodeJPEG(img, q)
			if candidate != nil && int64(len(candidate)) < int64(len(best.data)) {
				best = encodedImage{data: candidate, mimeType: "image/jpeg"}
			}
			if int64(len(best.data)) <= config.MaxSizeBytes {
				break
			}
		}
	}

	return best
}

// resizeAndEncode resizes the image if needed, then re-encodes it through a
// multi-step ladder (multi-format selection → quality ladder → dimension
// scale ladder). When the resize was triggered by dimension overflow, the
// result is always used even if it is larger than the original, because
// oversized images are rejected by vision model APIs. When triggered only by
// file size, the result is used only if it is actually smaller.
func resizeAndEncode(img image.Image, originalData []byte, originalMimeType string, originalSize int64, config CompressionConfig) (*CompressResult, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	dimensionOver := width > config.MaxDimension || height > config.MaxDimension

	if dimensionOver {
		img = imaging.Fit(img, config.MaxDimension, config.MaxDimension, imaging.Lanczos)
	}

	hasTransparency := hasAlpha(img)

	// Step 1+2: multi-format selection + quality ladder at full size.
	best := encodeWithLadder(img, hasTransparency, config)

	// Step 3: dimension scale ladder — progressively shrink if still over budget.
	if best.data != nil && int64(len(best.data)) > config.MaxSizeBytes {
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		for _, scale := range scaleSteps {
			sw := int(float64(w) * scale)
			sh := int(float64(h) * scale)
			if sw < 100 || sh < 100 {
				break
			}
			scaled := imaging.Resize(img, sw, sh, imaging.Lanczos)
			candidate := encodeWithLadder(scaled, hasTransparency, config)
			if candidate.data != nil && int64(len(candidate.data)) < int64(len(best.data)) {
				best = candidate
			}
			if int64(len(best.data)) <= config.MaxSizeBytes {
				break
			}
		}
	}

	// Guard: if all encoding attempts failed, return original.
	if best.data == nil {
		slog.Warn("All image encoding attempts failed, returning original")
		return &CompressResult{
			Data:           originalData,
			MimeType:       originalMimeType,
			WasCompressed:  false,
			OriginalSize:   originalSize,
			CompressedSize: originalSize,
		}, nil
	}

	compressedSize := int64(len(best.data))

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
		"output_format", best.mimeType,
		"dimension_resized", dimensionOver,
	)

	return &CompressResult{
		Data:           best.data,
		MimeType:       best.mimeType,
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
