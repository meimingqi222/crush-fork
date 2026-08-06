package imageutil

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
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

	// Create a JPEG image with random noise that exceeds the compression
	// threshold. 1000x1000 with high-entropy pixels at quality 100 produces
	// ~2 MB, well above the 1 MB MaxSizeBytes limit. Using direct pixel
	// access instead of img.Set avoids 1M color-model conversions.
	size := 1000
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
			pix[i+3] = 255
		}
	}

	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100})
	data := buf.Bytes()

	if int64(len(data)) <= config.MaxSizeBytes {
		t.Skip("Test image not large enough to trigger compression")
	}

	result, err := CompressImage(data, "image/jpeg", config)
	if err != nil {
		t.Fatalf("CompressImage() error = %v", err)
	}

	if result.WasCompressed {
		if result.MimeType != "image/jpeg" {
			t.Errorf("CompressImage() mimeType = %v, want image/jpeg", result.MimeType)
		}
		if result.CompressedSize >= result.OriginalSize {
			t.Error("CompressImage() should reduce size when compressing")
		}
	}
}

func TestCompressImage_PreserveTransparency(t *testing.T) {
	config := DefaultCompressionConfig()

	// Create a PNG with random noise and semi-transparency. 600x600 with
	// high-entropy pixels produces ~1.1 MB, exceeding the 1 MB threshold so
	// the test actually exercises the compression path (the previous
	// 2000x2000 low-entropy pattern compressed to only 51 KB and was always
	// skipped).
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

	if config.MaxDimension != 2048 {
		t.Errorf("Default MaxDimension = %v, want 2048", config.MaxDimension)
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
