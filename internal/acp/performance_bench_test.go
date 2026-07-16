package acp

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
)

func BenchmarkACPTextDelta(b *testing.B) {
	server := NewServerWithIO(nil, nil, nil)
	params := SessionUpdateNotification{
		SessionID: "session-benchmark",
		Update: SessionUpdate{
			SessionUpdate: SessionUpdateAgentMessageChunk,
			Content:       &ContentBlock{Type: "text", Text: "representative streaming delta"},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !server.writeLineAsync(mustMarshalBenchmarkNotification(b, params)) {
			req := <-server.writeAsyncCh
			server.releaseQueuedBytes(&server.asyncBytes, len(req.data), server.asyncSpace)
			requireBenchmarkEnqueue(b, server.writeLineAsync(mustMarshalBenchmarkNotification(b, params)))
		}
		req := <-server.writeAsyncCh
		server.releaseQueuedBytes(&server.asyncBytes, len(req.data), server.asyncSpace)
	}
}

func BenchmarkSessionSnapshot(b *testing.B) {
	snapshot := benchmarkSnapshot(20)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(snapshot); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionMessagePage(b *testing.B) {
	page := benchmarkSnapshot(200).Messages
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTerminalOutputCoalescing(b *testing.B) {
	chunks := make([][]byte, 32)
	for i := range chunks {
		chunks[i] = []byte(fmt.Sprintf("terminal output line %04d\r\n", i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buffer := make([]byte, 0, 1024)
		for _, chunk := range chunks {
			buffer = append(buffer, chunk...)
		}
	}
}

func BenchmarkLongSessionLoad(b *testing.B) {
	messages := benchmarkSnapshot(10_000).Messages
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// Establish the cost of the current full-history projection. The GUI
		// snapshot path introduced by WP-06 must remain independent of this.
		if _, err := json.Marshal(messages); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConcurrentSessions(b *testing.B) {
	var sequences [10]atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		index := 0
		for pb.Next() {
			sequences[index%len(sequences)].Add(1)
			index++
		}
	})
}

type benchmarkSessionSnapshot struct {
	SessionID      string                    `json:"sessionId"`
	Status         string                    `json:"status"`
	LatestSequence uint64                    `json:"latestSequence"`
	Messages       []benchmarkMessageSummary `json:"messages"`
}

type benchmarkMessageSummary struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func benchmarkSnapshot(messageCount int) benchmarkSessionSnapshot {
	messages := make([]benchmarkMessageSummary, messageCount)
	for i := range messages {
		messages[i] = benchmarkMessageSummary{
			ID:      fmt.Sprintf("message-%08d", i),
			Role:    "assistant",
			Content: "Representative persisted message content for desktop performance baselines.",
		}
	}
	return benchmarkSessionSnapshot{
		SessionID:      "session-benchmark",
		Status:         "idle",
		LatestSequence: uint64(messageCount),
		Messages:       messages,
	}
}

func mustMarshalBenchmarkNotification(b *testing.B, params SessionUpdateNotification) []byte {
	b.Helper()
	payload, err := json.Marshal(params)
	if err != nil {
		b.Fatal(err)
	}
	raw, err := json.Marshal(Request{JSONRPC: "2.0", Method: "session/update", Params: payload})
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

func requireBenchmarkEnqueue(b *testing.B, ok bool) {
	b.Helper()
	if !ok {
		b.Fatal("benchmark notification queue remained full")
	}
}
