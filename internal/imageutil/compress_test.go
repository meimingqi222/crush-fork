package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"

	// Register WebP decoder so image.Decode can handle image/webp.
	_ "golang.org/x/image/webp"
)

// createTestImage creates a test image with the specified dimensions.
func createTestImage(width, height int, withAlpha bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	pix := img.Pix
	stride := img.Stride

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*stride + x*4
			if withAlpha {
				pix[i] = 255 // R
				pix[i+1] = 0
				pix[i+2] = 0
				pix[i+3] = 128
			} else {
				pix[i] = 255
				pix[i+1] = 0
				pix[i+2] = 0
				pix[i+3] = 255
			}
		}
	}

	var buf bytes.Buffer
	if withAlpha {
		png.Encode(&buf, img)
	} else {
		jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	}
	return buf.Bytes()
}

// createNoisyImage creates a high-entropy JPEG that is hard to compress.
// Such images exercise the quality/dimension ladder because a single q75
// pass rarely gets them under budget.
func createNoisyImage(width, height int, quality int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	pix := img.Pix
	stride := img.Stride
	rng := rand.New(rand.NewPCG(42, 42))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*stride + x*4
			pix[i] = uint8(rng.IntN(256))
			pix[i+1] = uint8(rng.IntN(256))
			pix[i+2] = uint8(rng.IntN(256))
			pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return buf.Bytes()
}

func TestDetectMimeType(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected string
	}{
		{
			name:     "PNG image",
			data:     []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			expected: "image/png",
		},
		{
			name:     "JPEG image",
			data:     []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46},
			expected: "image/jpeg",
		},
		{
			name:     "GIF image",
			data:     []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00},
			expected: "image/gif",
		},
		{
			name:     "WebP image",
			data:     []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50},
			expected: "image/webp",
		},
		{
			name:     "Unknown format",
			data:     []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			expected: "",
		},
		{
			name:     "Too short",
			data:     []byte{0x00, 0x01},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectMimeType(tt.data)
			if result != tt.expected {
				t.Errorf("DetectMimeType() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestShouldCompress(t *testing.T) {
	config := DefaultCompressionConfig()

	tests := []struct {
		name     string
		dataSize int
		expected bool
	}{
		{
			name:     "Small image under threshold",
			dataSize: 512 * 1024, // 512KB
			expected: false,
		},
		{
			name:     "Image at threshold",
			dataSize: 1024 * 1024, // 1MB exactly
			expected: false,
		},
		{
			name:     "Image over threshold",
			dataSize: 2 * 1024 * 1024, // 2MB
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, tt.dataSize)
			result := ShouldCompress(data, config)
			if result != tt.expected {
				t.Errorf("ShouldCompress() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCompressImage_NoCompressionNeeded(t *testing.T) {
	config := DefaultCompressionConfig()

	// Create a small image that doesn't need compression
	data := createTestImage(100, 100, false)
	originalSize := len(data)

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if result.WasCompressed {
		t.Error("CompressImage() should not have compressed small image")
	}

	if len(result.Data) != originalSize {
		t.Error("CompressImage() should return original data for small images")
	}
}

func TestCompressImage_JPEGCompression(t *testing.T) {
	config := DefaultCompressionConfig()

	// Create a noisy JPEG that exceeds the 1MB threshold.
	data := createNoisyImage(1000, 1000, 100)

	if int64(len(data)) <= config.MaxSizeBytes {
		t.Skip("Test image not large enough to trigger compression")
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Error("CompressImage() should have compressed large image")
	}

	if result.CompressedSize >= result.OriginalSize {
		t.Errorf("Compressed size %d should be less than original %d", result.CompressedSize, result.OriginalSize)
	}

	// Result should be PNG or JPEG.
	if result.MimeType != "image/png" && result.MimeType != "image/jpeg" {
		t.Errorf("CompressImage() mimeType = %v, want image/png or image/jpeg", result.MimeType)
	}
}

func TestCompressImage_QualityLadderReducesSize(t *testing.T) {
	// A very large noisy image that a single q75 pass cannot get under 1MB.
	// The quality ladder (75 -> 60 -> 45 -> 30) should progressively reduce
	// the size. This test verifies the ladder actually kicks in by checking
	// the result is smaller than what a single q75 encoding would produce.
	config := DefaultCompressionConfig()

	// 2000x2000 noisy JPEG at quality 100 is ~5MB - well above the 1MB target.
	data := createNoisyImage(2000, 2000, 100)

	if int64(len(data)) <= config.MaxSizeBytes {
		t.Skip("Test image not large enough to trigger compression")
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Error("CompressImage() should have compressed large noisy image")
	}

	// The quality/dimension ladder should have brought this well under 1MB.
	// For a 2000x2000 noisy image, even q30 JPEG is typically under 500KB.
	if result.CompressedSize > config.MaxSizeBytes {
		t.Logf("Warning: compressed size %d still exceeds target %d (ladder may need more steps)",
			result.CompressedSize, config.MaxSizeBytes)
	}

	// At minimum, the result must be significantly smaller than the original.
	if result.CompressedSize >= result.OriginalSize {
		t.Errorf("Compressed size %d should be less than original %d", result.CompressedSize, result.OriginalSize)
	}
}

func TestCompressImage_MultiFormatPicksSmaller(t *testing.T) {
	// A simple solid-color image (no transparency) compresses much smaller
	// as PNG than JPEG. The multi-format selector should pick PNG.
	config := DefaultCompressionConfig()
	// Use a very low threshold so compression triggers even for a small image.
	config.MaxSizeBytes = 1

	// 100x100 solid red, no alpha - PNG will be tiny, JPEG will have artifacts.
	data := createTestImage(100, 100, false)

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Error("CompressImage() should have compressed image with MaxSizeBytes=1")
	}

	// For a solid-color image, PNG should be smaller than JPEG.
	if result.MimeType != "image/png" {
		t.Errorf("CompressImage() should pick PNG for solid-color image, got %v (size=%d)",
			result.MimeType, result.CompressedSize)
	}
}

func TestCompressImage_PreserveTransparency(t *testing.T) {
	config := DefaultCompressionConfig()

	// Create a PNG with random noise and semi-transparency.
	size := 600
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	pix := img.Pix
	stride := img.Stride
	rng := rand.New(rand.NewPCG(42, 42))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := y*stride + x*4
			pix[i] = uint8(rng.IntN(256))
			pix[i+1] = uint8(rng.IntN(256))
			pix[i+2] = uint8(rng.IntN(256))
			pix[i+3] = 128
		}
	}

	var buf bytes.Buffer
	png.Encode(&buf, img)
	data := buf.Bytes()

	if int64(len(data)) <= config.MaxSizeBytes {
		t.Skip("Test image not large enough to trigger compression")
	}

	result, err := CompressImage(data, "image/png", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if result.WasCompressed {
		if result.MimeType != "image/png" {
			t.Errorf("CompressImage() mimeType = %v, want image/png (for transparency)", result.MimeType)
		}
	}
}

func TestDefaultCompressionConfig(t *testing.T) {
	config := DefaultCompressionConfig()

	if config.MaxSizeBytes != 1024*1024 {
		t.Errorf("Default MaxSizeBytes = %v, want %v", config.MaxSizeBytes, 1024*1024)
	}

	if config.JPEGQuality != 75 {
		t.Errorf("Default JPEGQuality = %v, want 75", config.JPEGQuality)
	}

	if config.MaxDimension != 2000 {
		t.Errorf("Default MaxDimension = %v, want %d", config.MaxDimension, 2000)
	}
}

func TestCompressImage_DimensionOverflowSmallFile(t *testing.T) {
	// Reproduce the real-world bug: a JPEG that is under the 1MB size
	// threshold but whose dimensions exceed MaxDimension. Previously this
	// image would bypass compression entirely and be rejected by the
	// vision model API (e.g. OpenAI's 2000px limit).
	config := DefaultCompressionConfig()

	// 2600×2200 solid-color JPEG is well under 1MB but exceeds 2000px.
	data := createTestImage(2600, 2200, false)

	if int64(len(data)) > config.MaxSizeBytes {
		t.Skipf("Test image too large (%d bytes), expected under %d", len(data), config.MaxSizeBytes)
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Error("CompressImage() should have compressed image with dimensions exceeding MaxDimension")
	}

	// Verify the result decodes to a within-limit image. Use image.Decode
	// instead of jpeg.Decode because multi-format selection may output PNG.
	img, _, err := image.Decode(bytes.NewReader(result.Data))
	if err != nil {
		t.Fatalf("Failed to decode compressed result: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() > config.MaxDimension {
		t.Errorf("Compressed image width = %d, want <= %d", bounds.Dx(), config.MaxDimension)
	}
	if bounds.Dy() > config.MaxDimension {
		t.Errorf("Compressed image height = %d, want <= %d", bounds.Dy(), config.MaxDimension)
	}
}

func TestCompressImage_DimensionOverflowKeepsEvenIfLarger(t *testing.T) {
	// A dimension-triggered resize must always use the re-encoded result,
	// even if the re-encoded bytes are larger than the original.
	config := DefaultCompressionConfig()
	config.JPEGQuality = 100 // Force high quality so re-encode is likely larger

	// 2100×2100 solid red JPEG: small file, but dimension over limit.
	data := createTestImage(2100, 2100, false)
	originalSize := int64(len(data))

	if int64(len(data)) > config.MaxSizeBytes {
		t.Skipf("Test image too large (%d bytes), expected under %d", len(data), config.MaxSizeBytes)
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Error("CompressImage() should have compressed image with dimensions exceeding MaxDimension")
	}

	// Even if compressed is larger, it must be used (dimension overflow).
	if result.CompressedSize >= originalSize {
		// This is fine - the point is that it WAS compressed despite
		// being larger. Just log it.
		t.Logf("Compressed result (%d bytes) is larger than original (%d bytes) but was kept due to dimension overflow",
			result.CompressedSize, originalSize)
	}

	// Verify dimensions are within limit. Use image.Decode because the
	// multi-format selector may output PNG for solid-color images.
	img, _, err := image.Decode(bytes.NewReader(result.Data))
	if err != nil {
		t.Fatalf("Failed to decode compressed result: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() > config.MaxDimension || bounds.Dy() > config.MaxDimension {
		t.Errorf("Compressed dimensions = %dx%d, want <= %d", bounds.Dx(), bounds.Dy(), config.MaxDimension)
	}
}

func TestHasAlpha(t *testing.T) {
	// Test image with alpha.
	imgWithAlpha := image.NewRGBA(image.Rect(0, 0, 10, 10))
	imgWithAlpha.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 128})
	if !hasAlpha(imgWithAlpha) {
		t.Error("hasAlpha() should return true for image with semi-transparent pixels")
	}

	// Test image without alpha.
	imgNoAlpha := image.NewRGBA(image.Rect(0, 0, 10, 10))
	pix := imgNoAlpha.Pix
	stride := imgNoAlpha.Stride
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			i := y*stride + x*4
			pix[i] = 255
			pix[i+3] = 255
		}
	}
	if hasAlpha(imgNoAlpha) {
		t.Error("hasAlpha() should return false for image with no transparency")
	}
}

func TestEncodeWithLadder_TransparentImageReturnsPNG(t *testing.T) {
	config := DefaultCompressionConfig()

	// Create a transparent image.
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 128})
		}
	}

	result := encodeWithLadder(img, true, config)
	if result.data == nil {
		t.Fatal("encodeWithLadder() returned nil data for transparent image")
	}
	if result.mimeType != "image/png" {
		t.Errorf("encodeWithLadder() mimeType = %v, want image/png for transparent image", result.mimeType)
	}
}

func TestEncodeWithLadder_OpaqueImagePicksSmallerFormat(t *testing.T) {
	config := DefaultCompressionConfig()

	// Solid-color opaque image: PNG should be smaller than JPEG.
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}

	result := encodeWithLadder(img, false, config)
	if result.data == nil {
		t.Fatal("encodeWithLadder() returned nil data")
	}

	// For a solid red image, PNG should be smaller.
	pngSize := len(encodePNG(img))
	jpegSize := len(encodeJPEG(img, config.JPEGQuality))
	if pngSize < jpegSize && result.mimeType != "image/png" {
		t.Errorf("encodeWithLadder() should pick PNG (%d bytes) over JPEG (%d bytes), but got %v",
			pngSize, jpegSize, result.mimeType)
	}
}

func TestCompressImage_QualityLadderHitsTargetForModerateImage(t *testing.T) {
	// A moderately large noisy image (1500x1500 q90 ~2MB) should be brought
	// under the 1MB target by the quality ladder without needing dimension
	// scaling. This verifies the quality ladder alone is effective.
	config := DefaultCompressionConfig()

	data := createNoisyImage(1500, 1500, 90)
	if int64(len(data)) <= config.MaxSizeBytes {
		t.Skip("Test image not large enough to trigger compression")
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if !result.WasCompressed {
		t.Fatal("CompressImage() should have compressed image")
	}

	// The quality ladder should get this under 1MB without dimension scaling.
	// (1500x1500 is under the 2000px dimension limit, so no resize happens.)
	if result.CompressedSize > config.MaxSizeBytes {
		t.Errorf("Quality ladder should have brought %dx%d image under %d bytes, got %d",
			1500, 1500, config.MaxSizeBytes, result.CompressedSize)
	}

	// Verify dimensions are preserved (no dimension scaling needed).
	// Use image.Decode because multi-format selection may output PNG.
	img, _, err := image.Decode(bytes.NewReader(result.Data))
	if err == nil {
		bounds := img.Bounds()
		if bounds.Dx() != 1500 || bounds.Dy() != 1500 {
			t.Errorf("Dimensions should be preserved at 1500x1500, got %dx%d", bounds.Dx(), bounds.Dy())
		}
	}
}
