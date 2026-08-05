package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	file := `D:\code\copilot-refs\crush-refs\crush\internal\agent\tools\edit.go`
	data, err := os.ReadFile(file)
	if err != nil {
		panic(err)
	}
	content := string(data)

	// 1. Add FileHashlineOperations type and FileOperations field to EditParams
	// Add the new type before EditParams
	oldType := "type EditPermissionsParams struct {"
	newType := "// FileHashlineOperations groups hashline operations targeting a single file.\r\n// Used by the file_operations parameter to edit multiple files atomically in one call.\r\ntype FileHashlineOperations struct {\r\n\tFilePath   string                  `json:\"file_path\" description:\"The absolute path to the file to modify\"`\r\n\tOperations []HashlineEditOperation `json:\"operations\" description:\"Array of hashline operations to apply to this file\"`\r\n}\r\n\r\ntype EditPermissionsParams struct {"
	if !strings.Contains(content, oldType) {
		fmt.Println("ERROR: EditPermissionsParams not found")
		return
	}
	content = strings.ReplaceAll(content, oldType, newType)

	// Add FileOperations field to EditParams (after Patch field)
	oldPatch := "\tPatch      string                  `json:\"patch,omitempty\" description:\"Unified diff format patch containing changes to apply to files\"`\r\n}"
	newPatch := "\tPatch           string                    `json:\"patch,omitempty\" description:\"Unified diff format patch containing changes to apply to files\"`\r\n\tFileOperations  []FileHashlineOperations  `json:\"file_operations,omitempty\" description:\"Array of per-file hashline operations for multi-file atomic edits. Each entry specifies a file_path and its operations array. When provided, all other parameters are ignored.\"`\r\n}"
	if !strings.Contains(content, oldPatch) {
		fmt.Println("ERROR: Patch field not found")
		return
	}
	content = strings.ReplaceAll(content, oldPatch, newPatch)

	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		panic(err)
	}
	fmt.Println("Types added")
}
