package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/lsp/util"
	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

// HandleWorkspaceConfiguration handles workspace configuration requests
func HandleWorkspaceConfiguration(_ context.Context, _ string, params json.RawMessage) (any, error) {
	return []map[string]any{{}}, nil
}

// HandleRegisterCapability handles capability registration requests
func HandleRegisterCapability(_ context.Context, _ string, params json.RawMessage) (any, error) {
	var registerParams protocol.RegistrationParams
	if err := json.Unmarshal(params, &registerParams); err != nil {
		slog.Error("Error unmarshaling registration params", "error", err)
		return nil, err
	}

	for _, reg := range registerParams.Registrations {
		switch reg.Method {
		case "workspace/didChangeWatchedFiles":
			// Parse the registration options
			optionsJSON, err := json.Marshal(reg.RegisterOptions)
			if err != nil {
				slog.Error("Error marshaling registration options", "error", err)
				continue
			}
			var options protocol.DidChangeWatchedFilesRegistrationOptions
			if err := json.Unmarshal(optionsJSON, &options); err != nil {
				slog.Error("Error unmarshaling registration options", "error", err)
				continue
			}
			// Store the file watchers registrations
			notifyFileWatchRegistration(reg.ID, options.Watchers)
		}
	}
	return nil, nil
}

// HandleApplyEdit handles workspace edit requests
func HandleApplyEdit(encoding powernap.OffsetEncoding) func(_ context.Context, _ string, params json.RawMessage) (any, error) {
	return func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var edit protocol.ApplyWorkspaceEditParams
		if err := json.Unmarshal(params, &edit); err != nil {
			return nil, err
		}

		err := util.ApplyWorkspaceEdit(edit.Edit, encoding)
		if err != nil {
			slog.Error("Error applying workspace edit", "error", err)
			return protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: err.Error()}, nil
		}

		return protocol.ApplyWorkspaceEditResult{Applied: true}, nil
	}
}

// FileWatchRegistrationHandler is a function that will be called when file watch registrations are received
type FileWatchRegistrationHandler func(id string, watchers []protocol.FileSystemWatcher)

// fileWatchHandler holds the current handler for file watch registrations
var fileWatchHandler FileWatchRegistrationHandler

// RegisterFileWatchHandler sets the handler for file watch registrations
func RegisterFileWatchHandler(handler FileWatchRegistrationHandler) {
	fileWatchHandler = handler
}

// notifyFileWatchRegistration notifies the handler about new file watch registrations
func notifyFileWatchRegistration(id string, watchers []protocol.FileSystemWatcher) {
	if fileWatchHandler != nil {
		fileWatchHandler(id, watchers)
	}
}

// HandleServerMessage handles server messages
func HandleServerMessage(_ context.Context, method string, params json.RawMessage) {
	var msg protocol.ShowMessageParams
	if err := json.Unmarshal(params, &msg); err != nil {
		slog.Debug("Error unmarshal server message", "error", err)
		return
	}

	switch msg.Type {
	case protocol.Error:
		slog.Error("LSP Server", "message", msg.Message)
	case protocol.Warning:
		slog.Warn("LSP Server", "message", msg.Message)
	case protocol.Info:
		slog.Info("LSP Server", "message", msg.Message)
	case protocol.Log:
		slog.Debug("LSP Server", "message", msg.Message)
	}
}

// HandleDiagnostics handles diagnostic notifications from the LSP server
func HandleDiagnostics(client *Client, params json.RawMessage) {
	var diagParams protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &diagParams); err != nil {
		slog.Error("Error unmarshaling diagnostics params", "error", err)
		return
	}

	client.diagnostics.Set(diagParams.URI, diagParams.Diagnostics)

	// Calculate total diagnostic count
	totalCount := 0
	for _, diagnostics := range client.diagnostics.Seq2() {
		totalCount += len(diagnostics)
	}

	// Trigger callback if set
	if client.onDiagnosticsChanged != nil {
		client.onDiagnosticsChanged(client.name, totalCount)
	}
}

// parseToken converts the raw JSON message token to its formatted string representation safely.
func parseToken(token json.RawMessage) string {
	var tokenVal any
	dec := json.NewDecoder(bytes.NewReader(token))
	dec.UseNumber()
	if err := dec.Decode(&tokenVal); err == nil {
		switch v := tokenVal.(type) {
		case json.Number:
			return v.String()
		default:
			return fmt.Sprintf("%v", v)
		}
	}
	return strings.Trim(string(token), `"`)
}

// HandleWorkDoneProgressCreate handles window/workDoneProgress/create
// requests from the LSP server.
func HandleWorkDoneProgressCreate(client *Client, _ context.Context, _ string, params json.RawMessage) (any, error) {
	var p struct {
		Token json.RawMessage `json:"token"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}

	tokenStr := parseToken(p.Token)

	client.progressMu.Lock()
	if _, exists := client.progresses[tokenStr]; !exists {
		client.progresses[tokenStr] = &ProgressInfo{}
	}
	client.progressMu.Unlock()

	if client.onUpdate != nil {
		client.onUpdate()
	}

	return nil, nil
}

type progressValue struct {
	Kind       string  `json:"kind"` // "begin" | "report" | "end"
	Title      string  `json:"title,omitempty"`
	Message    string  `json:"message,omitempty"`
	Percentage float64 `json:"percentage,omitempty"`
}

// HandleProgressNotification handles $/progress notifications from the LSP server.
func HandleProgressNotification(client *Client, params json.RawMessage) {
	var p struct {
		Token json.RawMessage `json:"token"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}

	var val progressValue
	if err := json.Unmarshal(p.Value, &val); err != nil {
		return
	}

	tokenStr := parseToken(p.Token)

	client.progressMu.Lock()
	progressUpdated := false

	switch val.Kind {
	case "begin":
		if info, ok := client.progresses[tokenStr]; ok {
			info.Title = val.Title
			info.Message = val.Message
			info.Percentage = val.Percentage
		} else {
			client.progresses[tokenStr] = &ProgressInfo{
				Title:      val.Title,
				Message:    val.Message,
				Percentage: val.Percentage,
			}
		}
		progressUpdated = true
	case "report":
		if info, ok := client.progresses[tokenStr]; ok {
			if val.Title != "" && info.Title != val.Title {
				info.Title = val.Title
				progressUpdated = true
			}
			if val.Message != "" && info.Message != val.Message {
				info.Message = val.Message
				progressUpdated = true
			}
			if val.Percentage > 0 && info.Percentage != val.Percentage {
				info.Percentage = val.Percentage
				progressUpdated = true
			}
		}
	case "end":
		delete(client.progresses, tokenStr)
		progressUpdated = true
	}
	client.progressMu.Unlock()

	// Re-evaluate state after releasing the lock. SetServerState checks the
	// current progress token count under its own lock and auto-adjusts between
	// StateReady and StateIndexing, so a single call is sufficient regardless
	// of the notification kind.
	if progressUpdated {
		prevState := client.GetServerState()
		client.SetServerState(prevState)
	}
}
