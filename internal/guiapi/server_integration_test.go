package guiapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	"github.com/charmbracelet/crush/internal/guiapi"
	"github.com/charmbracelet/crush/internal/sessionevent"
	"github.com/stretchr/testify/require"
)

func TestStandardInitializeAdvertisesExtensionWithoutRequiringIt(t *testing.T) {
	t.Parallel()

	connection := newTestConnection(t)
	response := connection.call(t, "initialize", acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      acp.ClientInfo{Name: "standard-client"},
	})
	require.Nil(t, response.Error)

	var result acp.InitializeResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	require.Equal(t, acp.ProtocolVersion, result.ProtocolVersion)
	require.Equal(t, "crush", result.AgentInfo.Name)
	require.Contains(t, result.Experimental, "crush")

	standardResponse := connection.call(t, "session/cancel", acp.SessionCancelParams{SessionID: "not-running"})
	require.Nil(t, standardResponse.Error)
}

func TestServerRoutesNegotiatedCrushMethodOutsideACPHandler(t *testing.T) {
	t.Parallel()

	connection := newTestConnection(t)
	require.NoError(t, connection.service.Register("crush/fs/read", guiapi.FeatureClientFS, func(context.Context, json.RawMessage) (any, *acp.RPCError) {
		return map[string]string{"source": "guiapi"}, nil
	}))
	selection, err := json.Marshal(guiapi.Selection{
		ProtocolVersion: guiapi.ProtocolVersion,
		Features:        []guiapi.Feature{guiapi.FeatureClientFS},
	})
	require.NoError(t, err)
	initialize := connection.call(t, "initialize", acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      acp.ClientInfo{Name: "desktop-client"},
		Experimental:    map[string]json.RawMessage{"crush": selection},
	})
	require.Nil(t, initialize.Error)

	response := connection.call(t, "crush/fs/read", map[string]string{"path": "test"})
	require.Nil(t, response.Error)
	var result map[string]string
	require.NoError(t, json.Unmarshal(response.Result, &result))
	require.Equal(t, "guiapi", result["source"])
}

func TestServerRejectsUnnegotiatedKnownCrushMethod(t *testing.T) {
	t.Parallel()

	connection := newTestConnection(t)
	initialize := connection.call(t, "initialize", acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      acp.ClientInfo{Name: "standard-client"},
	})
	require.Nil(t, initialize.Error)

	response := connection.call(t, "crush/session/get", map[string]string{"sessionId": "session-1"})
	require.NotNil(t, response.Error)
	require.Equal(t, -32010, response.Error.Code)
	data, ok := response.Error.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "CRUSH_FEATURE_NOT_NEGOTIATED", data["code"])
}

func TestSubscribeResponsePrecedesReplayNotification(t *testing.T) {
	t.Parallel()

	events := sessionevent.NewHub(sessionevent.Config{})
	published, err := events.Publish("session-1", sessionevent.NewEvent{
		Kind:    sessionevent.KindMessageDelta,
		Payload: sessionevent.TextDelta{MessageID: "message-1", PartID: "part-1", Text: "hello"},
	})
	require.NoError(t, err)
	connection := newTestConnectionWithHub(t, events)
	selection, err := json.Marshal(guiapi.Selection{
		ProtocolVersion: guiapi.ProtocolVersion,
		Features:        []guiapi.Feature{guiapi.FeatureSessionSync},
	})
	require.NoError(t, err)
	initialize := connection.call(t, "initialize", acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      acp.ClientInfo{Name: "desktop-client"},
		Experimental:    map[string]json.RawMessage{"crush": selection},
	})
	require.Nil(t, initialize.Error)

	response := connection.call(t, "crush/session/subscribe", map[string]any{
		"sessionId":     "session-1",
		"afterSequence": 0,
	})
	require.Nil(t, response.Error)
	var subscription struct {
		SubscriptionID string `json:"subscriptionId"`
		LatestSequence uint64 `json:"latestSequence"`
	}
	require.NoError(t, json.Unmarshal(response.Result, &subscription))
	require.NotEmpty(t, subscription.SubscriptionID)
	require.Equal(t, published.Sequence, subscription.LatestSequence)

	require.True(t, connection.scanner.Scan(), "server closed before replay notification: %v", connection.scanner.Err())
	var notification struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			SubscriptionID string `json:"subscriptionId"`
			Event          struct {
				Sequence uint64 `json:"sequence"`
			} `json:"event"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(connection.scanner.Bytes(), &notification))
	require.Equal(t, "2.0", notification.JSONRPC)
	require.Equal(t, "crush/session/event", notification.Method)
	require.Equal(t, subscription.SubscriptionID, notification.Params.SubscriptionID)
	require.Equal(t, published.Sequence, notification.Params.Event.Sequence)
}

type testConnection struct {
	service *guiapi.Service
	input   *io.PipeWriter
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	nextID  int
}

func newTestConnection(t *testing.T) *testConnection {
	return newTestConnectionWithHub(t, nil)
}

func newTestConnectionWithHub(t *testing.T, events *sessionevent.Hub) *testConnection {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	handler := acp.NewHandler(nil)
	service := guiapi.NewService(events)
	handler.SetExperimentalExtension(service)
	server := acp.NewServerWithIO(handler, inReader, outWriter)
	server.SetExtensionRouter(service)
	service.SetNotificationWriter(server)
	handler.SetServer(server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() { _ = server.Serve(ctx) }()

	connection := &testConnection{
		service: service,
		input:   inWriter,
		scanner: bufio.NewScanner(outReader),
		cancel:  cancel,
	}
	connection.scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	t.Cleanup(func() {
		service.Close()
		cancel()
		_ = inWriter.Close()
		_ = outReader.Close()
	})
	return connection
}

func (c *testConnection) call(t *testing.T, method string, params any) acp.Response {
	t.Helper()
	c.nextID++
	paramsJSON, err := json.Marshal(params)
	require.NoError(t, err)
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  method,
		"params":  json.RawMessage(paramsJSON),
	}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	_, err = fmt.Fprintf(c.input, "%s\n", raw)
	require.NoError(t, err)
	require.True(t, c.scanner.Scan(), "server closed before response: %v", c.scanner.Err())
	var response acp.Response
	require.NoError(t, json.Unmarshal(c.scanner.Bytes(), &response))
	return response
}
