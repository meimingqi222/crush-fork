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

func handleArchiveRead(ctx context.Context, uri string, sel pathSelector, archiveDir string, tracker filetracker.Service) (fantasy.ToolResponse, error) {
	if archiveDir == "" {
		return fantasy.NewTextErrorResponse("archive directory is not configured"), nil
	}
	id := strings.TrimPrefix(uri, "archive://")
	if id == "" || !archiveIDPattern.MatchString(id) {
		return fantasy.NewTextErrorResponse("invalid archive reference: ID must contain only hexadecimal characters"), nil
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("archive not found: %s", uri)), nil
		}
		return fantasy.ToolResponse{}, fmt.Errorf("error reading archive directory: %w", err)
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
	if len(matches) == 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("archive not found: %s", uri)), nil
	}
	if len(matches) > 1 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("archive reference is ambiguous: %s", uri)), nil
	}
	filePath, err := filepath.Abs(matches[0])
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error resolving archive path: %w", err)
	}
	archiveRoot, err := filepath.Abs(archiveDir)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error resolving archive directory: %w", err)
	}
	rel, err := filepath.Rel(archiveRoot, filePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Ext(rel) != ".txt" {
		return fantasy.NewTextErrorResponse("invalid archive path"), nil
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
