package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/message"
)

const minCacheFootprintTokens = 2048

// computeToolSignature fingerprints the tool catalog and MCP instructions that
// affect the provider-visible prefix. Rebuild enhanced system prompt when this
// changes even if the base template string is unchanged.
func computeToolSignature(tools []fantasy.AgentTool) string {
	names := make([]string, 0, len(tools))
	descs := make([]string, 0, len(tools))
	for _, t := range tools {
		info := t.Info()
		names = append(names, info.Name)
		descs = append(descs, fmt.Sprintf("%s=%s", info.Name, info.Description))
	}
	slices.Sort(names)
	slices.Sort(descs)

	var mcpSeg strings.Builder
	mcpStates := mcp.GetStates()
	mcpNames := make([]string, 0, len(mcpStates))
	for name := range mcpStates {
		mcpNames = append(mcpNames, name)
	}
	slices.Sort(mcpNames)
	for _, name := range mcpNames {
		server := mcpStates[name]
		if server.State != mcp.StateConnected {
			continue
		}
		if s := server.Client.InitializeResult().Instructions; s != "" {
			mcpSeg.WriteString(name)
			mcpSeg.WriteByte('=')
			mcpSeg.WriteString(s)
			mcpSeg.WriteByte('\n')
		}
	}

	h := sha256.New()
	h.Write([]byte(strings.Join(names, "\x01")))
	h.Write([]byte{'\x02'})
	h.Write([]byte(strings.Join(descs, "\x02")))
	h.Write([]byte{'\x03'})
	h.Write([]byte(mcpSeg.String()))
	return hex.EncodeToString(h.Sum(nil))
}

func promptDateUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

// DetectCacheInvalidation reports a warm→cold Anthropic-style prompt cache
// transition between two assistant turns.
func DetectCacheInvalidation(prev, current message.Usage) (reprocessed int64, ok bool) {
	if prev.CacheReadTokens < minCacheFootprintTokens {
		return 0, false
	}
	if current.CacheReadTokens > 0 {
		return 0, false
	}
	if current.CacheWriteTokens <= 0 {
		return 0, false
	}
	reprocessed = current.CacheWriteTokens + current.InputTokens
	if reprocessed < minCacheFootprintTokens {
		return 0, false
	}
	return reprocessed, true
}
