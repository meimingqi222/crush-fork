package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// ArtifactWriter handles filesystem I/O for materialized views.
// It is a pure file writer without store/search semantics. Each materializer
// uses one ArtifactWriter to write its output files.
type ArtifactWriter struct {
	outputDir string
}

// NewArtifactWriter creates an ArtifactWriter that writes files under
// outputDir (typically <data_dir>/memory/).
func NewArtifactWriter(outputDir string) *ArtifactWriter {
	return &ArtifactWriter{outputDir: outputDir}
}

// WriteFile writes content to a file relative to the output directory.
// Parent directories are created automatically.
func (w *ArtifactWriter) WriteFile(name string, content []byte) error {
	path := filepath.Join(w.outputDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", name, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// RemoveFile removes a file relative to the output directory.
func (w *ArtifactWriter) RemoveFile(name string) error {
	path := filepath.Join(w.outputDir, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// OutputDir returns the base output directory.
func (w *ArtifactWriter) OutputDir() string {
	return w.outputDir
}
