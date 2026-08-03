package openaicompat

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/google/uuid"
)

const maxDSMLBufferSize = 1 << 20

var (
	dsmlOpenTokens = []string{
		"<｜DSML｜tool_calls",
		"<|DSML|tool_calls",
	}
	dsmlOpenRegex           = regexp.MustCompile(`(?i)<[|｜]DSML[|｜]tool_calls\s*>`)
	dsmlCloseRegex          = regexp.MustCompile(`(?i)</[|｜]DSML[|｜]tool_calls\s*>`)
	dsmlInvokeRegex         = regexp.MustCompile(`(?is)<[|｜]DSML[|｜]invoke\s+([^>]*)>(.*?)</[|｜]DSML[|｜]invoke\s*>`)
	dsmlParameterRegex      = regexp.MustCompile(`(?is)<[|｜]DSML[|｜]parameter\s+([^>]*)>(.*?)</[|｜]DSML[|｜]parameter\s*>`)
	dsmlParameterStartRegex = regexp.MustCompile(`(?is)<[|｜]DSML[|｜]parameter\b`)
	dsmlAttributeRegex      = regexp.MustCompile(`(?i)\b([a-zA-Z_][a-zA-Z0-9_-]*)\s*=\s*"([^"]*)"`)
)

type dsmlCall struct {
	name  string
	input string
}

type dsmlScanEvent struct {
	text  string
	calls []dsmlCall
}

type dsmlScanner struct {
	buffer    string
	capturing bool
}

func (s *dsmlScanner) feed(delta string, final bool) []dsmlScanEvent {
	s.buffer += delta
	events := make([]dsmlScanEvent, 0, 2)
	for s.buffer != "" {
		if s.capturing {
			closeAt := dsmlCloseRegex.FindStringIndex(s.buffer)
			if closeAt == nil {
				if !final && len(s.buffer) <= maxDSMLBufferSize {
					break
				}
				events = appendTextEvent(events, s.buffer)
				s.buffer = ""
				s.capturing = false
				break
			}
			block := s.buffer[:closeAt[1]]
			s.buffer = s.buffer[closeAt[1]:]
			s.capturing = false
			calls, ok := parseDSMLBlock(block)
			if !ok {
				events = appendTextEvent(events, block)
				continue
			}
			events = append(events, dsmlScanEvent{calls: calls})
			continue
		}

		openAt := dsmlOpenRegex.FindStringIndex(s.buffer)
		if openAt != nil {
			events = appendTextEvent(events, s.buffer[:openAt[0]])
			s.buffer = s.buffer[openAt[0]:]
			s.capturing = true
			continue
		}
		if final {
			events = appendTextEvent(events, s.buffer)
			s.buffer = ""
			break
		}

		hold := dsmlPartialOpenLength(s.buffer)
		emitLen := len(s.buffer) - hold
		if emitLen == 0 {
			break
		}
		events = appendTextEvent(events, s.buffer[:emitLen])
		s.buffer = s.buffer[emitLen:]
		break
	}
	return events
}

func appendTextEvent(events []dsmlScanEvent, text string) []dsmlScanEvent {
	if text == "" {
		return events
	}
	if len(events) > 0 && len(events[len(events)-1].calls) == 0 {
		events[len(events)-1].text += text
		return events
	}
	return append(events, dsmlScanEvent{text: text})
}

func dsmlPartialOpenLength(text string) int {
	for _, token := range dsmlOpenTokens {
		if at := strings.LastIndex(text, token); at >= 0 && strings.TrimSpace(text[at+len(token):]) == "" {
			return len(text) - at
		}
	}
	maxLength := 0
	for _, token := range dsmlOpenTokens {
		limit := min(len(text), len(token)-1)
		for length := 1; length <= limit; length++ {
			if strings.EqualFold(text[len(text)-length:], token[:length]) {
				maxLength = max(maxLength, length)
			}
		}
	}
	return maxLength
}

func parseDSMLBlock(block string) ([]dsmlCall, bool) {
	invokeMatches := dsmlInvokeRegex.FindAllStringSubmatch(block, -1)
	if len(invokeMatches) == 0 {
		return nil, false
	}
	body := dsmlOpenRegex.ReplaceAllString(block, "")
	body = dsmlCloseRegex.ReplaceAllString(body, "")
	if strings.TrimSpace(dsmlInvokeRegex.ReplaceAllString(body, "")) != "" {
		return nil, false
	}
	calls := make([]dsmlCall, 0, len(invokeMatches))
	seen := make(map[string]struct{}, len(invokeMatches))
	for _, invoke := range invokeMatches {
		attributes := parseDSMLAttributes(invoke[1])
		name := strings.TrimSpace(attributes["name"])
		if name == "" {
			return nil, false
		}
		parameterMatches := dsmlParameterRegex.FindAllStringSubmatch(invoke[2], -1)
		if len(parameterMatches) != len(dsmlParameterStartRegex.FindAllString(invoke[2], -1)) ||
			strings.TrimSpace(dsmlParameterRegex.ReplaceAllString(invoke[2], "")) != "" {
			return nil, false
		}
		input := make(map[string]any, len(parameterMatches))
		for _, parameter := range parameterMatches {
			parameterAttributes := parseDSMLAttributes(parameter[1])
			parameterName := strings.TrimSpace(parameterAttributes["name"])
			if parameterName == "" {
				return nil, false
			}
			value := strings.TrimSpace(html.UnescapeString(parameter[2]))
			if strings.EqualFold(parameterAttributes["string"], "true") {
				input[parameterName] = value
				continue
			}
			var decoded any
			if json.Unmarshal([]byte(value), &decoded) == nil {
				input[parameterName] = decoded
			} else {
				input[parameterName] = value
			}
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, false
		}
		key := name + "\x00" + string(encoded)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		calls = append(calls, dsmlCall{name: name, input: string(encoded)})
	}
	return calls, len(calls) > 0
}

func parseDSMLAttributes(source string) map[string]string {
	attributes := make(map[string]string)
	for _, match := range dsmlAttributeRegex.FindAllStringSubmatch(source, -1) {
		attributes[strings.ToLower(match[1])] = html.UnescapeString(match[2])
	}
	return attributes
}

type dsmlStreamChannel struct {
	start         fantasy.StreamPart
	deltaMetadata fantasy.ProviderMetadata
	scanner       dsmlScanner
	started       bool
	segment       int
	baseID        string
}

type dsmlStreamTransformer struct {
	yield          func(fantasy.StreamPart) bool
	channels       map[string]*dsmlStreamChannel
	pendingCalls   []dsmlCall
	recoveredCalls int
	nativeToolCall bool
	stopped        bool
}

// TransformDSMLStream removes leaked DSML from text and reasoning channels and
// emits equivalent structured tool-call events.
func TransformDSMLStream(stream fantasy.StreamResponse) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		transformer := dsmlStreamTransformer{
			yield:    yield,
			channels: make(map[string]*dsmlStreamChannel),
		}
		stream(func(part fantasy.StreamPart) bool {
			return transformer.handle(part)
		})
		if !transformer.stopped {
			transformer.flushChannels()
		}
	}
}

func (t *dsmlStreamTransformer) handle(part fantasy.StreamPart) bool {
	switch part.Type {
	case fantasy.StreamPartTypeTextStart, fantasy.StreamPartTypeReasoningStart:
		key := dsmlChannelKey(part.Type, part.ID)
		channel := &dsmlStreamChannel{start: part, baseID: part.ID}
		t.channels[key] = channel
		if part.Delta != "" {
			return t.emitEvents(channel, channel.scanner.feed(part.Delta, false))
		}
		return true
	case fantasy.StreamPartTypeTextDelta, fantasy.StreamPartTypeReasoningDelta:
		startType := fantasy.StreamPartTypeTextStart
		if part.Type == fantasy.StreamPartTypeReasoningDelta {
			startType = fantasy.StreamPartTypeReasoningStart
		}
		key := dsmlChannelKey(startType, part.ID)
		channel := t.channels[key]
		if channel == nil {
			channel = &dsmlStreamChannel{start: fantasy.StreamPart{Type: startType, ID: part.ID}, baseID: part.ID}
			t.channels[key] = channel
		}
		channel.deltaMetadata = part.ProviderMetadata
		return t.emitEvents(channel, channel.scanner.feed(part.Delta, false))
	case fantasy.StreamPartTypeTextEnd, fantasy.StreamPartTypeReasoningEnd:
		startType := fantasy.StreamPartTypeTextStart
		if part.Type == fantasy.StreamPartTypeReasoningEnd {
			startType = fantasy.StreamPartTypeReasoningStart
		}
		key := dsmlChannelKey(startType, part.ID)
		channel := t.channels[key]
		if channel == nil {
			return true
		}
		if !t.emitEvents(channel, channel.scanner.feed("", true)) || !t.endChannel(channel) {
			return false
		}
		delete(t.channels, key)
		return true
	case fantasy.StreamPartTypeFinish:
		if !t.flushChannels() {
			return false
		}
		if !t.nativeToolCall && !t.emitPendingCalls() {
			return false
		}
		if t.recoveredCalls > 0 && !t.nativeToolCall {
			part.FinishReason = fantasy.FinishReasonToolCalls
		}
		return t.emit(part)
	case fantasy.StreamPartTypeError:
		if !t.flushChannels() {
			return false
		}
		t.pendingCalls = nil
		return t.emit(part)
	case fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolCall:
		t.nativeToolCall = true
		t.pendingCalls = nil
		return t.emit(part)
	default:
		return t.emit(part)
	}
}

func dsmlChannelKey(startType fantasy.StreamPartType, id string) string {
	return string(startType) + "\x00" + id
}

func (t *dsmlStreamTransformer) emitEvents(channel *dsmlStreamChannel, events []dsmlScanEvent) bool {
	for _, event := range events {
		if event.text != "" {
			if !t.startChannel(channel) {
				return false
			}
			deltaType := fantasy.StreamPartTypeTextDelta
			if channel.start.Type == fantasy.StreamPartTypeReasoningStart {
				deltaType = fantasy.StreamPartTypeReasoningDelta
			}
			if !t.emit(fantasy.StreamPart{
				Type:             deltaType,
				ID:               channel.start.ID,
				Delta:            event.text,
				ProviderMetadata: channel.deltaMetadata,
			}) {
				return false
			}
		}
		if len(event.calls) == 0 {
			continue
		}
		if !t.endChannel(channel) {
			return false
		}
		if !t.nativeToolCall {
			t.pendingCalls = append(t.pendingCalls, event.calls...)
		}
	}
	return true
}

func (t *dsmlStreamTransformer) emitPendingCalls() bool {
	for _, call := range t.pendingCalls {
		t.recoveredCalls++
		id := "call_dsml_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if !t.emit(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: call.name}) ||
			!t.emit(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: call.input}) ||
			!t.emit(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) ||
			!t.emit(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: call.name, ToolCallInput: call.input}) {
			return false
		}
	}
	t.pendingCalls = nil
	return true
}

func (t *dsmlStreamTransformer) startChannel(channel *dsmlStreamChannel) bool {
	if channel.started {
		return true
	}
	channel.segment++
	channel.start.ID = channel.baseID
	if channel.segment > 1 {
		channel.start.ID += "-dsml-" + strconv.Itoa(channel.segment)
	}
	channel.start.Delta = ""
	channel.started = t.emit(channel.start)
	return channel.started
}

func (t *dsmlStreamTransformer) endChannel(channel *dsmlStreamChannel) bool {
	if !channel.started {
		return true
	}
	endType := fantasy.StreamPartTypeTextEnd
	if channel.start.Type == fantasy.StreamPartTypeReasoningStart {
		endType = fantasy.StreamPartTypeReasoningEnd
	}
	channel.started = false
	return t.emit(fantasy.StreamPart{Type: endType, ID: channel.start.ID})
}

func (t *dsmlStreamTransformer) flushChannels() bool {
	for key, channel := range t.channels {
		if !t.emitEvents(channel, channel.scanner.feed("", true)) || !t.endChannel(channel) {
			return false
		}
		delete(t.channels, key)
	}
	return true
}

func (t *dsmlStreamTransformer) emit(part fantasy.StreamPart) bool {
	if t.stopped {
		return false
	}
	if !t.yield(part) {
		t.stopped = true
		return false
	}
	return true
}
