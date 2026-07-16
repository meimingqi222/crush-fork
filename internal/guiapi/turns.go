package guiapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/acp"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/clientfs"
	"github.com/charmbracelet/crush/internal/idempotency"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/turn"
)

const (
	errorIdempotencyConflict = "CRUSH_IDEMPOTENCY_CONFLICT"
	errorSessionBusy         = "CRUSH_SESSION_BUSY"
	errorRevisionConflict    = "CRUSH_REVISION_CONFLICT"
	errorDeadlineExceeded    = "CRUSH_DEADLINE_EXCEEDED"
	errorTurnNotFound        = "CRUSH_TURN_NOT_FOUND"
	errorQueueFull           = "CRUSH_QUEUE_FULL"
	errorTurnFailed          = "CRUSH_TURN_FAILED"
	maxTurnWait              = 5 * time.Minute
)

type turnContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Filename string `json:"filename,omitempty"`
	BlobID   string `json:"blobId,omitempty"`
}

type turnStartParams struct {
	SessionID       string                     `json:"sessionId"`
	Content         []turnContentBlock         `json:"content"`
	Inference       session.InferenceOverrides `json:"inference,omitempty"`
	ClientRequestID string                     `json:"clientRequestId"`
}

type turnWaitParams struct {
	TurnID    string `json:"turnId"`
	TimeoutMS int64  `json:"timeoutMs,omitempty"`
}

type turnCancelParams struct {
	SessionID       string `json:"sessionId"`
	TurnID          string `json:"turnId"`
	ClientRequestID string `json:"clientRequestId"`
}

type queueListParams struct {
	SessionID string `json:"sessionId"`
}

type queueRemoveParams struct {
	SessionID       string `json:"sessionId"`
	TurnID          string `json:"turnId"`
	ClientRequestID string `json:"clientRequestId"`
}

type queueReorderParams struct {
	SessionID        string   `json:"sessionId"`
	ExpectedRevision uint64   `json:"expectedRevision"`
	TurnIDs          []string `json:"turnIds"`
	ClientRequestID  string   `json:"clientRequestId"`
}

type steerParams struct {
	SessionID       string             `json:"sessionId"`
	Content         []turnContentBlock `json:"content"`
	ClientRequestID string             `json:"clientRequestId"`
}

type retryParams struct {
	SessionID       string `json:"sessionId"`
	TurnID          string `json:"turnId,omitempty"`
	MessageID       string `json:"messageId,omitempty"`
	ClientRequestID string `json:"clientRequestId"`
}

type cancelResult struct {
	Acknowledged bool      `json:"acknowledged"`
	Turn         turn.Turn `json:"turn"`
}

type steerResult struct {
	Mode             string     `json:"mode"`
	AcceptedSequence uint64     `json:"acceptedSequence"`
	Turn             *turn.Turn `json:"turn,omitempty"`
}

func (s *Service) registerTurnHandlers() {
	s.routes["crush/turn/start"] = route{feature: FeatureSessionControl, handler: s.handleTurnStart}
	s.routes["crush/turn/wait"] = route{feature: FeatureSessionControl, handler: s.handleTurnWait}
	s.routes["crush/turn/cancel"] = route{feature: FeatureSessionControl, handler: s.handleTurnCancel}
	s.routes["crush/session/queue/list"] = route{feature: FeatureSessionControl, handler: s.handleQueueList}
	s.routes["crush/session/queue/remove"] = route{feature: FeatureSessionControl, handler: s.handleQueueRemove}
	s.routes["crush/session/queue/reorder"] = route{feature: FeatureSessionControl, handler: s.handleQueueReorder}
	s.routes["crush/session/steer"] = route{feature: FeatureSessionControl, handler: s.handleSteer}
	s.routes["crush/session/retry"] = route{feature: FeatureSessionControl, handler: s.handleRetry}
}

func (s *Service) handleTurnStart(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params turnStartParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	input, rpcErr := s.turnInput(ctx, params.SessionID, params.Content, params.Inference)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if !inferenceOverridesEmpty(params.Inference) {
		if rpcErr := s.validateInference(context.WithoutCancel(ctx), params.SessionID, params.Inference); rpcErr != nil {
			return nil, rpcErr
		}
	}
	return s.mutate(ctx, "crush/turn/start", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		value, err := turns.Start(context.Background(), params.SessionID, input)
		if err != nil {
			return nil, turnError(err)
		}
		return value, nil
	})
}

func inferenceOverridesEmpty(value session.InferenceOverrides) bool {
	return value.Model == "" && value.Provider == "" && value.MaxOutputTokens == nil &&
		value.Temperature == nil && value.TopP == nil && value.TopK == nil &&
		value.FrequencyPenalty == nil && value.PresencePenalty == nil && value.Think == nil
}

func (s *Service) handleTurnWait(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params turnWaitParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.TurnID == "" {
		return nil, invalidParams(errors.New("turnId is required"))
	}
	turns := s.turnService()
	if turns == nil {
		return nil, sourceUnavailable("turn service is unavailable")
	}
	waitCtx := ctx
	var cancel context.CancelFunc
	if params.TimeoutMS > 0 {
		duration := min(time.Duration(params.TimeoutMS)*time.Millisecond, maxTurnWait)
		waitCtx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	value, err := turns.Wait(waitCtx, params.TurnID)
	if err == nil {
		return value, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, &acp.RPCError{Code: -32031, Message: errorDeadlineExceeded, Data: ErrorData{Code: errorDeadlineExceeded, Retryable: true}}
	}
	if errors.Is(err, context.Canceled) {
		return nil, &acp.RPCError{Code: -32031, Message: errorDeadlineExceeded, Data: ErrorData{Code: errorDeadlineExceeded, Retryable: true}}
	}
	return nil, turnError(err)
}

func (s *Service) handleTurnCancel(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params turnCancelParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.TurnID == "" {
		return nil, invalidParams(errors.New("turnId is required"))
	}
	return s.mutate(ctx, "crush/turn/cancel", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		current, err := turns.Get(params.TurnID)
		if err != nil || current.SessionID != params.SessionID {
			return nil, turnError(turn.ErrNotFound)
		}
		value, err := turns.Cancel(params.TurnID)
		if err != nil {
			return nil, turnError(err)
		}
		return cancelResult{Acknowledged: true, Turn: value}, nil
	})
}

func (s *Service) handleQueueList(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params queueListParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if params.SessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	turns := s.turnService()
	if turns == nil {
		return nil, sourceUnavailable("turn service is unavailable")
	}
	return turns.Queue(params.SessionID), nil
}

func (s *Service) handleQueueRemove(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params queueRemoveParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	return s.mutate(ctx, "crush/session/queue/remove", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		queue, err := turns.RemoveQueued(params.SessionID, params.TurnID)
		if err != nil {
			return nil, turnError(err)
		}
		return queue, nil
	})
}

func (s *Service) handleQueueReorder(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params queueReorderParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	return s.mutate(ctx, "crush/session/queue/reorder", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		queue, err := turns.Reorder(params.SessionID, params.ExpectedRevision, params.TurnIDs)
		if err != nil {
			return nil, turnError(err)
		}
		return queue, nil
	})
}

func (s *Service) handleSteer(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params steerParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	input, rpcErr := s.turnInput(ctx, params.SessionID, params.Content, session.InferenceOverrides{})
	if rpcErr != nil {
		return nil, rpcErr
	}
	return s.mutate(ctx, "crush/session/steer", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		mode, queued, sequence, err := turns.Steer(params.SessionID, input)
		if err != nil {
			return nil, turnError(err)
		}
		return steerResult{Mode: mode, AcceptedSequence: sequence, Turn: queued}, nil
	})
}

func (s *Service) handleRetry(ctx context.Context, raw json.RawMessage) (any, *acp.RPCError) {
	var params retryParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, invalidParams(err)
	}
	if (params.TurnID == "") == (params.MessageID == "") {
		return nil, invalidParams(errors.New("exactly one of turnId or messageId is required"))
	}
	return s.mutate(ctx, "crush/session/retry", params.SessionID, params.ClientRequestID, params, func(turns *turn.Service) (any, *acp.RPCError) {
		var value turn.Turn
		var err error
		if params.TurnID != "" {
			current, getErr := turns.Get(params.TurnID)
			if getErr != nil || current.SessionID != params.SessionID {
				return nil, turnError(turn.ErrNotFound)
			}
			value, err = turns.RetryTurn(context.Background(), params.TurnID)
		} else {
			value, err = turns.RetryMessage(context.Background(), params.SessionID, params.MessageID)
		}
		if err != nil {
			return nil, turnError(err)
		}
		return value, nil
	})
}

func (s *Service) mutate(
	ctx context.Context,
	method, sessionID, requestID string,
	payload any,
	fn func(*turn.Service) (any, *acp.RPCError),
) (any, *acp.RPCError) {
	if sessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	s.mu.RLock()
	turns := s.turns
	sessions := s.sessions
	s.mu.RUnlock()
	if turns == nil {
		return nil, sourceUnavailable("turn mutation service is unavailable")
	}
	return s.executeMutation(ctx, method, sessionID, requestID, payload, func() (any, *acp.RPCError) {
		if sessions != nil {
			if _, err := sessions.Get(ctx, sessionID); err != nil {
				return nil, sessionSourceError(sessionID, err)
			}
		}
		return fn(turns)
	})
}

func (s *Service) executeMutation(
	ctx context.Context,
	method, sessionID, requestID string,
	payload any,
	fn func() (any, *acp.RPCError),
) (any, *acp.RPCError) {
	if sessionID == "" {
		return nil, invalidParams(errors.New("sessionId is required"))
	}
	s.mu.RLock()
	store := s.idempotency
	s.mu.RUnlock()
	if store == nil {
		return nil, sourceUnavailable("mutation idempotency service is unavailable")
	}
	outcome, err := store.Execute(ctx, method+"\x00"+sessionID, requestID, payload, func() idempotency.Outcome {
		value, rpcErr := fn()
		return idempotency.Outcome{Value: value, Failure: rpcErr}
	})
	if err != nil {
		if errors.Is(err, idempotency.ErrConflict) {
			return nil, protocolError(-32030, errorIdempotencyConflict, map[string]any{"clientRequestId": requestID})
		}
		if errors.Is(err, idempotency.ErrCapacity) {
			return nil, &acp.RPCError{Code: -32032, Message: errorSessionBusy, Data: ErrorData{Code: errorSessionBusy, Retryable: true}}
		}
		return nil, invalidParams(errors.New("clientRequestId must be a UUID"))
	}
	if rpcErr, _ := outcome.Failure.(*acp.RPCError); rpcErr != nil {
		return nil, rpcErr
	}
	return outcome.Value, nil
}

func (s *Service) turnService() *turn.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turns
}

func (s *Service) turnInput(ctx context.Context, sessionID string, blocks []turnContentBlock, inference session.InferenceOverrides) (turn.Input, *acp.RPCError) {
	if sessionID == "" {
		return turn.Input{}, invalidParams(errors.New("sessionId is required"))
	}
	var texts []string
	attachments := make([]message.Attachment, 0)
	references := make([]turn.AttachmentReference, 0)
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "image", "audio", "resource":
			if base64.StdEncoding.DecodedLen(len(block.Data)) > maxInlineBinaryBytes {
				return turn.Input{}, protocolError(-32036, errorPayloadTooLarge, map[string]any{"maxInlineBytes": maxInlineBinaryBytes})
			}
			data, err := base64.StdEncoding.DecodeString(block.Data)
			if err != nil {
				return turn.Input{}, invalidParams(fmt.Errorf("invalid %s data", block.Type))
			}
			attachments = append(attachments, message.Attachment{FilePath: block.Filename, MimeType: block.MIMEType, Content: data})
		case "blob":
			if block.BlobID == "" {
				return turn.Input{}, invalidParams(errors.New("blobId is required"))
			}
			blobID := block.BlobID
			references = append(references, turn.AttachmentReference{
				ID: blobID,
				Resolve: func(resolveCtx context.Context) (message.Attachment, error) {
					return s.resolveBlobAttachment(resolveCtx, sessionID, blobID)
				},
			})
		default:
			return turn.Input{}, invalidParams(fmt.Errorf("unsupported content type %q", block.Type))
		}
	}
	input := turn.Input{
		Prompt: strings.Join(texts, "\n"), Attachments: attachments,
		AttachmentReferences: references, Inference: inference,
	}
	s.mu.RLock()
	sessions := s.sessions
	mcpLifecycle := s.mcpLifecycle
	s.mu.RUnlock()
	var scopes []func(context.Context) context.Context
	if mcpLifecycle != nil {
		access := mcpLifecycle.Access(sessionID)
		scopes = append(scopes, func(runCtx context.Context) context.Context {
			return agenttools.WithMCPServerAccess(runCtx, access)
		})
	}
	if sessions != nil {
		if sess, err := sessions.Get(ctx, sessionID); err == nil {
			if scope := s.ClientFSForSession(sessionID, sess.WorkspaceCWD); scope != nil {
				scopes = append(scopes, func(runCtx context.Context) context.Context {
					return clientfs.WithScope(runCtx, scope)
				})
			}
		}
	}
	if len(scopes) > 0 {
		input.Scope = func(runCtx context.Context) context.Context {
			for _, apply := range scopes {
				runCtx = apply(runCtx)
			}
			return runCtx
		}
	}
	if input.Prompt == "" && len(input.Attachments) == 0 && len(input.AttachmentReferences) == 0 {
		return turn.Input{}, invalidParams(errors.New("content is empty"))
	}
	return input, nil
}

func turnError(err error) *acp.RPCError {
	switch {
	case errors.Is(err, turn.ErrNotFound):
		return protocolError(-32033, errorTurnNotFound, nil)
	case errors.Is(err, turn.ErrRevisionConflict):
		return protocolError(-32034, errorRevisionConflict, nil)
	case errors.Is(err, turn.ErrQueueFull):
		return &acp.RPCError{Code: -32035, Message: errorQueueFull, Data: ErrorData{Code: errorQueueFull, Retryable: true}}
	case errors.Is(err, turn.ErrInputTooLarge):
		return protocolError(-32036, "CRUSH_PAYLOAD_TOO_LARGE", nil)
	case errors.Is(err, turn.ErrInvalidOrder), errors.Is(err, turn.ErrNotQueued):
		return invalidParams(err)
	case errors.Is(err, turn.ErrRetrySource):
		return protocolError(-32037, errorTurnFailed, nil)
	default:
		return &acp.RPCError{Code: acp.CodeInternalError, Message: errorTurnFailed, Data: ErrorData{Code: errorTurnFailed, Retryable: true}}
	}
}
