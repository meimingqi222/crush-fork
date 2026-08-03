package tools

// LSPRestartParams describes an LSP client restart request. It is retained
// even though the standalone restart tool is unregistered: the UI layer
// renders historical restart tool messages and needs the type for JSON
// decoding.
type LSPRestartParams struct {
	// Name is the optional name of a specific LSP client to restart.
	// If empty, all LSP clients will be restarted.
	Name string `json:"name,omitempty"`
}

// LSPRestartToolName is kept for UI rendering of historical messages.
const LSPRestartToolName = "lsp_restart"
