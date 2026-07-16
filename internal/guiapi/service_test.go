package guiapi

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/stretchr/testify/require"
)

func TestCapabilitiesAreVersionedAndComplete(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	capabilities := service.ExperimentalCapabilities()
	capability, ok := capabilities["crush"].(Capability)
	require.True(t, ok)
	require.Equal(t, ProtocolVersion, capability.ProtocolVersion)
	require.Equal(t, supportedFeatures, capability.Features)
}

func TestNegotiationSelectsFeatureSubset(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureSessionSync, FeatureBlob},
	})))

	result, rpcErr := service.HandleExtension(t.Context(), "crush/protocol/status", nil)
	require.Nil(t, rpcErr)
	status := result.(statusResult)
	require.Equal(t, ProtocolVersion, status.ProtocolVersion)
	require.Equal(t, []Feature{FeatureBlob, FeatureSessionSync}, status.Features)
}

func TestNegotiationRejectsUnsupportedVersionAndFeature(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	rpcErr := service.NegotiateExperimental(experimentalSelection(t, Selection{ProtocolVersion: 99}))
	require.Equal(t, codeUnsupportedProtocol, rpcErr.Code)
	require.Equal(t, errorUnsupportedProtocol, rpcErr.Data.(ErrorData).Code)

	rpcErr = service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{"futureFeature"},
	}))
	require.Equal(t, codeUnsupportedFeature, rpcErr.Code)
	require.Equal(t, errorUnsupportedFeature, rpcErr.Data.(ErrorData).Code)
}

func TestFeatureGatingAndRegistration(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	var calls atomic.Int32
	require.NoError(t, service.Register("crush/fs/read", FeatureClientFS, func(context.Context, json.RawMessage) (any, *acp.RPCError) {
		calls.Add(1)
		return map[string]any{"ok": true}, nil
	}))

	_, rpcErr := service.HandleExtension(t.Context(), "crush/fs/read", nil)
	require.Equal(t, codeFeatureNotNegotiated, rpcErr.Code)
	require.Equal(t, int32(0), calls.Load())

	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureBlob},
	})))
	_, rpcErr = service.HandleExtension(t.Context(), "crush/fs/read", nil)
	require.Equal(t, codeFeatureNotNegotiated, rpcErr.Code)
	require.Equal(t, int32(0), calls.Load())

	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureClientFS},
	})))
	result, rpcErr := service.HandleExtension(t.Context(), "crush/fs/read", nil)
	require.Nil(t, rpcErr)
	require.Equal(t, map[string]any{"ok": true}, result)
	require.Equal(t, int32(1), calls.Load())
}

func TestOmittedCrushCapabilityResetsNegotiation(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureSessionSync},
	})))
	require.Nil(t, service.NegotiateExperimental(map[string]json.RawMessage{"other": json.RawMessage(`{}`)}))

	_, rpcErr := service.HandleExtension(t.Context(), "crush/protocol/status", nil)
	require.Equal(t, codeFeatureNotNegotiated, rpcErr.Code)
}

func TestInvalidRenegotiationFailsClosed(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	require.Nil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{
		ProtocolVersion: ProtocolVersion,
		Features:        []Feature{FeatureSessionSync},
	})))
	require.NotNil(t, service.NegotiateExperimental(experimentalSelection(t, Selection{ProtocolVersion: 99})))

	_, rpcErr := service.HandleExtension(t.Context(), "crush/session/get", nil)
	require.Equal(t, codeFeatureNotNegotiated, rpcErr.Code)
}

func TestNegotiationAndRoutingAreRaceSafe(t *testing.T) {
	t.Parallel()

	service := NewService(nil)
	require.NoError(t, service.Register("crush/fs/read", FeatureClientFS, func(context.Context, json.RawMessage) (any, *acp.RPCError) {
		return struct{}{}, nil
	}))
	selection := experimentalSelection(t, Selection{ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureClientFS}})

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			_ = service.NegotiateExperimental(selection)
		})
		wg.Go(func() {
			_, _ = service.HandleExtension(context.Background(), "crush/fs/read", nil)
		})
	}
	wg.Wait()
}

func experimentalSelection(t testing.TB, selection Selection) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(selection)
	require.NoError(t, err)
	return map[string]json.RawMessage{"crush": raw}
}

func BenchmarkNegotiatedRoute(b *testing.B) {
	service := NewService(nil)
	selection, err := json.Marshal(Selection{ProtocolVersion: ProtocolVersion, Features: []Feature{FeatureClientFS}})
	if err != nil {
		b.Fatal(err)
	}
	if rpcErr := service.NegotiateExperimental(map[string]json.RawMessage{"crush": selection}); rpcErr != nil {
		b.Fatal(rpcErr)
	}
	if err := service.Register("crush/fs/read", FeatureClientFS, func(context.Context, json.RawMessage) (any, *acp.RPCError) {
		return struct{}{}, nil
	}); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, rpcErr := service.HandleExtension(ctx, "crush/fs/read", nil); rpcErr != nil {
			b.Fatal(rpcErr)
		}
	}
}
