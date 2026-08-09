package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/filetracker"
)

var archiveIDPattern = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// resolveArchiveFilePath resolves an archive:// URI to the underlying .txt file
// on disk. It returns an error message suitable for surfacing to the model when
// the URI is malformed, the archive directory is unconfigured, or no unique
// archive matches. The resolved path is guaranteed to be inside archiveDir.
func resolveArchiveFilePath(uri, archiveDir string) (string, error) {
	id := strings.TrimPrefix(uri, "archive://")
	if id == "" || !archiveIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid archive reference: ID must contain only hexadecimal characters")
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("archive not found: %s", uri)
		}
		return "", fmt.Errorf("error reading archive directory: %w", err)
	}
	id = strings.ToLower(id)
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".txt" {
			continue
		}
		name := strings.TrimSuffix(strings.ToLower(entry.Name()), ".txt")
		if strings.HasPrefix(name, id) {
			matches = append(matches, filepath.Join(archiveDir, entry.Name()))
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("archive not found: %s", uri)
	case 1:
		// ok
	default:
		return "", fmt.Errorf("archive reference is ambiguous: %s", uri)
	}
	filePath, err := filepath.Abs(matches[0])
	if err != nil {
		return "", fmt.Errorf("error resolving archive path: %w", err)
	}
	archiveRoot, err := filepath.Abs(archiveDir)
	if err != nil {
		return "", fmt.Errorf("error resolving archive directory: %w", err)
	}
	rel, err := filepath.Rel(archiveRoot, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Ext(rel) != ".txt" {
		return "", fmt.Errorf("invalid archive path")
	}
	return filePath, nil
}

func handleArchiveRead(ctx context.Context, uri string, sel pathSelector, archiveDir string, tracker filetracker.Service) (fantasy.ToolResponse, error) {
	if archiveDir == "" {
		return fantasy.NewTextErrorResponse("archive directory is not configured"), nil
	}
	filePath, err := resolveArchiveFilePath(uri, archiveDir)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	readLimit := sel.limit
	if readLimit <= 0 {
		readLimit = DefaultReadLimit
	}
	readResult, err := readTextFileLines(filePath, sel.offset, readLimit)
	if err != nil {
		if err == errReadOffsetBeyondEOF {
			return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(fmt.Sprintf("Line %d is beyond end of file (%d lines total).", sel.offset+1, readResult.Total)), ReadResponseMetadata{Path: uri, TotalLines: readResult.Total}), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("error reading archive: %w", err)
	}
	lines := readResult.Lines
	content := joinDisplayLines(lines)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("Archive content is not valid UTF-8"), nil
	}
	useHashline := sel.hasLineSel && !sel.raw
	var output string
	if sel.raw {
		output = content + "\n"
		// Raw mode is verbatim, so it has no line-number footer; still surface
		// silent truncation at the default line limit so the caller knows to
		// paginate instead of assuming it saw the whole archive.
		if readResult.HasMore {
			nextLine := sel.offset + len(lines) + 1
			output += fmt.Sprintf("\n(Truncated after line %d. Use path=%q to continue reading.)\n", sel.offset+len(lines), fmt.Sprintf("%s:%d", uri, nextLine))
		}
	} else {
		output = "<file>\n"
		if useHashline {
			output += addHashlineLineNumbers(lines, sel.offset+1)
		} else {
			output += addLineNumbers(lines, sel.offset+1)
		}
		nextLine := sel.offset + len(lines) + 1
		if len(lines) > 0 {
			startLine := sel.offset + 1
			endLine := sel.offset + len(lines)
			if readResult.HasMore {
				output += fmt.Sprintf("\n\n(Showing lines %d-%d. Use path=%q to continue.)", startLine, endLine, fmt.Sprintf("%s:%d", uri, nextLine))
			} else if readResult.TotalKnown {
				output += fmt.Sprintf("\n\n(Showing lines %d-%d. End of file - total %d lines.)", startLine, endLine, readResult.Total)
			}
		} else if readResult.TotalKnown {
			output += fmt.Sprintf("\n\n(End of file - total %d lines.)", readResult.Total)
		}
		output += "\n</file>\n"
	}
	if tracker != nil {
		tracker.RecordRead(ctx, GetSessionFromContext(ctx), filePath)
	}
	meta := ReadResponseMetadata{Path: uri, Content: content, Hashline: useHashline, StartLine: sel.offset + 1}
	if readResult.TotalKnown {
		meta.TotalLines = readResult.Total
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), meta), nil
}
