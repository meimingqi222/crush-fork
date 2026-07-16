package agent

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/crush/internal/sessionevent"
)

const maxToolFileMetadata = 50

type clientFSToolMetadata struct {
	Path      string `json:"path"`
	FilePath  string `json:"file_path"`
	SourceURI string `json:"source_uri"`
	Revision  string `json:"revision"`
}

func clientFSFilesFromMetadata(raw string) []sessionevent.ToolFile {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var values []clientFSToolMetadata
	if strings.HasPrefix(raw, "[") {
		if json.Unmarshal([]byte(raw), &values) != nil {
			return nil
		}
	} else {
		var value clientFSToolMetadata
		if json.Unmarshal([]byte(raw), &value) != nil {
			return nil
		}
		values = []clientFSToolMetadata{value}
	}
	if len(values) > maxToolFileMetadata {
		values = values[:maxToolFileMetadata]
	}
	files := make([]sessionevent.ToolFile, 0, len(values))
	for _, value := range values {
		if value.Revision == "" || value.SourceURI == "" {
			continue
		}
		path := value.Path
		if path == "" {
			path = value.FilePath
		}
		files = append(files, sessionevent.ToolFile{
			Path: path, SourceURI: value.SourceURI, Revision: value.Revision,
		})
	}
	return files
}
