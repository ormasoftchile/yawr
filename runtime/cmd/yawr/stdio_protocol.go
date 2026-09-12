package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	internalgovernance "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/internal/serve"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/runstate"
)

const stdioProtocolVersion = "yawr.stdio/v1"
const maxStdioCommandBytes = 1024 * 1024
const maxStdioFrameBytes = 1024 * 1024

type stdioProtocol struct {
	reader       *bufio.Reader
	inputCloser  io.Closer
	encoder      *json.Encoder
	writeMu      sync.Mutex
	terminalSent bool

	mu       sync.RWMutex
	runID    string
	broker   *serve.PromptBroker
	handle   engine.RunHandle
	secrets  []string
	redactor *internalgovernance.Redactor
	protect  engine.DebugProtection

	commandResults      chan stdioCommandResult
	readStop            chan struct{}
	pumpOnce            sync.Once
	stopOnce            sync.Once
	readOnce            sync.Once
	eventsDone          chan struct{}
	eventsStarted       bool
	interactionsDone    chan struct{}
	interactionsStarted bool
	eventsErr           error
}

type stdioCommandResult struct {
	command stdioCommand
	err     error
}

type stdioCommand struct {
	Type   string                `json:"type"`
	RunID  string                `json:"runID,omitempty"`
	TurnID string                `json:"turnID,omitempty"`
	Reason string                `json:"reason,omitempty"`
	Answer json.RawMessage       `json:"answer,omitempty"`
	Inputs map[string]string     `json:"inputs,omitempty"`
	Debug  *serve.DebugRunConfig `json:"debug,omitempty"`
}

type stdioRunConfig struct {
	Inputs map[string]string
	Debug  *serve.DebugRunConfig
}

func newStdioProtocol(input io.Reader, output io.Writer) *stdioProtocol {
	protocol := &stdioProtocol{
		reader:           bufio.NewReaderSize(input, 64*1024),
		encoder:          json.NewEncoder(output),
		commandResults:   make(chan stdioCommandResult, 1),
		readStop:         make(chan struct{}),
		eventsDone:       make(chan struct{}),
		interactionsDone: make(chan struct{}),
	}
	protocol.inputCloser, _ = input.(io.Closer)
	return protocol
}

func protocolOutput(enabled bool) io.Writer {
	if enabled {
		return io.Discard
	}
	return nil
}

func (p *stdioProtocol) attach(
	ctx context.Context,
	runID string,
	broker *serve.PromptBroker,
	handle engine.RunHandle,
) error {
	if runID == "" {
		return fmt.Errorf("stdio protocol: runID is required")
	}
	p.mu.Lock()
	p.runID = runID
	p.broker = broker
	p.handle = handle
	p.mu.Unlock()

	p.readOnce.Do(func() { go p.readCommands(ctx) })
	if broker != nil {
		frames, err := broker.Subscribe(runID, 0)
		if err != nil {
			return fmt.Errorf("stdio protocol: subscribe interactions: %w", err)
		}
		p.mu.Lock()
		p.interactionsStarted = true
		p.mu.Unlock()
		go func() {
			defer close(p.interactionsDone)
			p.forwardInteractions(ctx, runID, broker, frames)
		}()
	}
	if handle != nil {
		p.mu.Lock()
		p.eventsStarted = true
		p.mu.Unlock()
		go func() {
			defer close(p.eventsDone)
			p.forwardEvents(ctx, runID, handle.Events())
		}()
	}
	return nil
}

func (p *stdioProtocol) waitForEvents(ctx context.Context) error {
	p.mu.RLock()
	started := p.eventsStarted
	p.mu.RUnlock()
	if !started {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.eventsDone:
		p.mu.RLock()
		err := p.eventsErr
		p.mu.RUnlock()
		return err
	}
}

func (p *stdioProtocol) waitForInteractions(ctx context.Context) error {
	p.mu.RLock()
	started := p.interactionsStarted
	p.mu.RUnlock()
	if !started {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.interactionsDone:
		p.mu.RLock()
		err := p.eventsErr
		p.mu.RUnlock()
		return err
	}
}

func (p *stdioProtocol) send(frame map[string]any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.terminalSent {
		return nil
	}
	frame["version"] = stdioProtocolVersion
	safeFrame, err := p.sanitizeFrame(frame)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(safeFrame)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maxStdioFrameBytes {
		return fmt.Errorf("stdio protocol frame exceeds %d bytes", maxStdioFrameBytes)
	}
	err = p.encoder.Encode(safeFrame)
	if err == nil && frame["type"] == "run.finished" {
		p.terminalSent = true
	}
	return err
}

func (p *stdioProtocol) configureRedaction(secretValues []string, patterns []*governance.RedactionPattern) error {
	return p.setRedaction(engine.DebugProtection{SecretValues: secretValues, RedactionPatterns: patterns}, true)
}

func (p *stdioProtocol) extendRedaction(additional engine.DebugProtection) error {
	return p.setRedaction(additional, false)
}

func (p *stdioProtocol) setRedaction(additional engine.DebugProtection, replace bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	protection := p.protect
	if replace {
		protection = engine.DebugProtection{}
	}
	protection = engine.MergeDebugProtection(protection, additional)
	redactor, err := internalgovernance.NewRedactor(protection.RedactionPatterns)
	if err != nil {
		return err
	}
	secrets := append([]string(nil), protection.SecretValues...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	p.secrets = secrets
	p.redactor = redactor
	p.protect = protection
	return nil
}

func (p *stdioProtocol) sanitizeFrame(frame map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > maxStdioFrameBytes {
		return nil, fmt.Errorf("stdio protocol frame exceeds %d bytes", maxStdioFrameBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	p.mu.RLock()
	secrets := append([]string(nil), p.secrets...)
	redactor := p.redactor
	p.mu.RUnlock()
	decodedFrame, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stdio protocol frame must be an object")
	}
	safe := make(map[string]any, len(decodedFrame))
	for key, value := range decodedFrame {
		switch key {
		case "type", "version", "runID", "turnID", "status", "stepsTruncated", "stepsOmitted":
			safe[key] = value
		case "event":
			safe[key] = sanitizeStdioEvent(value, secrets, redactor)
		case "interaction":
			safe[key] = sanitizeStdioInteraction(value, secrets, redactor)
		case "steps":
			safe[key] = sanitizeStdioSteps(value, secrets, redactor)
		case "error":
			safe[key] = sanitizeStdioProtocolError(value, secrets, redactor)
		case "routeTest":
			safe[key] = sanitizeStdioRouteTest(value, secrets, redactor)
		default:
			safe[key] = sanitizeStdioValue(value, secrets, redactor)
		}
	}
	return safe, nil
}

func sanitizeStdioEvent(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	event, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(event))
	for key, item := range event {
		if key == "payload" {
			safe[key] = sanitizeStdioEventPayload(item, secrets, redactor)
		} else {
			safe[key] = item
		}
	}
	return safe
}

func sanitizeStdioEventPayload(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	payload, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(payload))
	for key, item := range payload {
		switch key {
		case "run_id", "step_id", "node_id", "parent_step_id", "parent_kind", "include_alias", "branch_label",
			"kind", "status", "phase", "stream", "sequence", "invocation", "attempt", "duration_ms",
			"index", "total", "iteration", "dispatched", "call_path":
			safe[key] = item
		default:
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioInteraction(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	interaction, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(interaction))
	for key, item := range interaction {
		switch key {
		case "type", "id", "turnID", "runID", "stepID", "nodeID", "kind", "correlationID", "multiple", "min", "max":
			safe[key] = item
		case "debug":
			safe[key] = sanitizeStdioDebug(item, secrets, redactor)
		case "options":
			safe[key] = sanitizeStdioTokenItems(item, "value", secrets, redactor)
		case "routes":
			safe[key] = sanitizeStdioTokenItems(item, "label", secrets, redactor)
		case "fields":
			safe[key] = sanitizeStdioFields(item, secrets, redactor)
		case "host_action":
			safe[key] = sanitizeStdioHostAction(item, secrets, redactor)
		default:
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioDebug(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	debug, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(debug))
	for key, item := range debug {
		switch key {
		case "phase", "callPath", "call_path", "invocation", "attempt", "protectedVariables", "protected_vars", "outputProtected", "output_protected", "canStepInto", "can_step_into":
			safe[key] = item
		default:
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioTokenItems(value any, tokenKey string, secrets []string, redactor *internalgovernance.Redactor) any {
	items, ok := value.([]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make([]any, len(items))
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			safe[index] = sanitizeStdioValue(item, secrets, redactor)
			continue
		}
		copy := make(map[string]any, len(object))
		for key, field := range object {
			if key == tokenKey {
				copy[key] = field
			} else {
				copy[key] = sanitizeStdioValue(field, secrets, redactor)
			}
		}
		safe[index] = copy
	}
	return safe
}

func sanitizeStdioFields(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	items, ok := value.([]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make([]any, len(items))
	for index, item := range items {
		field, ok := item.(map[string]any)
		if !ok {
			safe[index] = sanitizeStdioValue(item, secrets, redactor)
			continue
		}
		copy := make(map[string]any, len(field))
		for key, fieldValue := range field {
			switch key {
			case "name", "type", "required":
				copy[key] = fieldValue
			case "options":
				copy[key] = sanitizeStdioTokenItems(fieldValue, "value", secrets, redactor)
			default:
				copy[key] = sanitizeStdioValue(fieldValue, secrets, redactor)
			}
		}
		safe[index] = copy
	}
	return safe
}

func sanitizeStdioHostAction(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	action, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(action))
	for key, item := range action {
		if key == "capability" {
			safe[key] = item
		} else {
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioSteps(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	steps, ok := value.([]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make([]any, len(steps))
	for index, item := range steps {
		step, ok := item.(map[string]any)
		if !ok {
			safe[index] = sanitizeStdioValue(item, secrets, redactor)
			continue
		}
		copy := make(map[string]any, len(step))
		for key, field := range step {
			switch key {
			case "step_id", "node_id", "kind", "status", "duration_ms", "started_at", "completed_at":
				copy[key] = field
			default:
				copy[key] = sanitizeStdioValue(field, secrets, redactor)
			}
		}
		safe[index] = copy
	}
	return safe
}

func sanitizeStdioProtocolError(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	protocolError, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(protocolError))
	for key, item := range protocolError {
		if key == "code" {
			safe[key] = item
		} else {
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioRouteTest(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	routeTest, ok := value.(map[string]any)
	if !ok {
		return sanitizeStdioValue(value, secrets, redactor)
	}
	safe := make(map[string]any, len(routeTest))
	for key, item := range routeTest {
		switch key {
		case "passed", "targetReached", "externalDispatches", "status":
			safe[key] = item
		default:
			safe[key] = sanitizeStdioValue(item, secrets, redactor)
		}
	}
	return safe
}

func sanitizeStdioValue(value any, secrets []string, redactor *internalgovernance.Redactor) any {
	switch typed := value.(type) {
	case string:
		return sanitizeStdioString(typed, secrets, redactor)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = sanitizeStdioValue(item, secrets, redactor)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = sanitizeStdioValue(item, secrets, redactor)
		}
		return result
	default:
		return typed
	}
}

func sanitizeStdioString(value string, secrets []string, redactor *internalgovernance.Redactor) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "<redacted>")
	}
	if redactor != nil {
		value, _ = redactor.RedactString(value)
	}
	return value
}

func (p *stdioProtocol) sendError(code, message string) {
	p.handleOutputFailure(p.send(map[string]any{
		"type": "protocol.error",
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}))
}

func (p *stdioProtocol) handleOutputFailure(err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	if p.eventsErr == nil {
		p.eventsErr = err
	}
	p.mu.Unlock()
	p.cancelActive("stdio protocol output failed")
}

func (p *stdioProtocol) readDebugConfig(ctx context.Context) (serve.DebugRunConfig, error) {
	config, err := p.readRunConfig(ctx)
	if err != nil {
		return serve.DebugRunConfig{}, err
	}
	if config.Debug == nil || !config.Debug.Enabled {
		return serve.DebugRunConfig{}, fmt.Errorf("stdio protocol: enabled debug configuration is required")
	}
	return *config.Debug, nil
}

func (p *stdioProtocol) readRunConfig(ctx context.Context) (stdioRunConfig, error) {
	command, err := p.nextCommand(ctx)
	if err != nil {
		return stdioRunConfig{}, fmt.Errorf("stdio protocol: read run configuration: %w", err)
	}
	if command.Type != "run.configure" {
		return stdioRunConfig{}, fmt.Errorf("stdio protocol: expected run.configure, got %q", command.Type)
	}
	if len(command.Inputs) > 64 {
		return stdioRunConfig{}, fmt.Errorf("stdio protocol: private inputs exceed 64 fields")
	}
	inputs := make(map[string]string, len(command.Inputs))
	for name, value := range command.Inputs {
		if name == "" || len([]byte(name)) > 256 || len([]byte(value)) > 64*1024 {
			return stdioRunConfig{}, fmt.Errorf("stdio protocol: invalid private input")
		}
		inputs[name] = value
	}
	return stdioRunConfig{Inputs: inputs, Debug: command.Debug}, nil
}

func (p *stdioProtocol) startCommandPump() {
	p.pumpOnce.Do(func() {
		go func() {
			defer close(p.commandResults)
			for {
				command, err := p.decodeCommand()
				select {
				case p.commandResults <- stdioCommandResult{command: command, err: err}:
				case <-p.readStop:
					return
				}
				if err != nil {
					return
				}
			}
		}()
	})
}

func (p *stdioProtocol) nextCommand(ctx context.Context) (stdioCommand, error) {
	p.startCommandPump()
	select {
	case <-ctx.Done():
		p.stopCommandPump()
		return stdioCommand{}, ctx.Err()
	case result, ok := <-p.commandResults:
		if !ok {
			return stdioCommand{}, io.EOF
		}
		return result.command, result.err
	}
}

func (p *stdioProtocol) stopCommandPump() {
	p.stopOnce.Do(func() {
		close(p.readStop)
		if p.inputCloser != nil {
			_ = p.inputCloser.Close()
		}
	})
}

func (p *stdioProtocol) decodeCommand() (stdioCommand, error) {
	line, err := p.readCommandLine()
	if err != nil {
		return stdioCommand{}, err
	}
	var command stdioCommand
	if err := json.Unmarshal(line, &command); err != nil {
		return stdioCommand{}, err
	}
	return command, nil
}

func (p *stdioProtocol) readCommandLine() ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := p.reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxStdioCommandBytes {
			return nil, fmt.Errorf("stdio protocol command exceeds %d bytes", maxStdioCommandBytes)
		}
		line = append(line, fragment...)
		switch err {
		case nil:
			return line, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func (p *stdioProtocol) readCommands(ctx context.Context) {
	for {
		command, err := p.nextCommand(ctx)
		if err != nil {
			if err == io.EOF && ctx.Err() == nil {
				p.cancelActive("stdio input closed")
			} else if ctx.Err() == nil {
				p.sendError("INVALID_COMMAND", err.Error())
				p.cancelActive("stdio protocol command failed")
			}
			return
		}
		if err := p.dispatchCommand(ctx, command); err != nil {
			p.sendError("COMMAND_REJECTED", err.Error())
		}
	}
}

func (p *stdioProtocol) cancelActive(reason string) {
	p.mu.RLock()
	handle := p.handle
	p.mu.RUnlock()
	if handle != nil {
		_ = handle.Cancel(context.Background(), reason)
	}
}

func (p *stdioProtocol) dispatchCommand(ctx context.Context, command stdioCommand) error {
	p.mu.RLock()
	runID := p.runID
	broker := p.broker
	handle := p.handle
	p.mu.RUnlock()

	if command.Type != "run.configure" && command.RunID == "" {
		return fmt.Errorf("runID is required")
	}
	if command.RunID != "" && command.RunID != runID {
		return fmt.Errorf("runID %q does not match active run %q", command.RunID, runID)
	}
	switch command.Type {
	case "interaction.answer":
		if broker == nil {
			return fmt.Errorf("interaction broker is not available")
		}
		if command.TurnID == "" {
			return fmt.Errorf("turnID is required")
		}
		var answer serve.AnswerEnvelope
		if len(command.Answer) == 0 {
			return fmt.Errorf("answer is required")
		}
		if err := json.Unmarshal(command.Answer, &answer); err != nil {
			return fmt.Errorf("decode answer: %w", err)
		}
		if err := broker.Answer(runID, command.TurnID, answer); err != nil {
			return fmt.Errorf("answer interaction: %w", err)
		}
		return nil
	case "run.cancel":
		if handle == nil {
			return fmt.Errorf("run handle is not available")
		}
		reason := command.Reason
		if reason == "" {
			reason = "operator cancelled"
		}
		return handle.Cancel(ctx, reason)
	default:
		return fmt.Errorf("unsupported command type %q", command.Type)
	}
}

func (p *stdioProtocol) forwardInteractions(
	ctx context.Context,
	runID string,
	broker *serve.PromptBroker,
	frames <-chan any,
) {
	defer broker.Unsubscribe(runID, frames)
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			switch interaction := frame.(type) {
			case serve.PendingInteraction:
				if err := p.send(map[string]any{
					"type":        "interaction.pending",
					"runID":       runID,
					"turnID":      interaction.TurnID,
					"interaction": serve.PreviewInteractionFrame(interaction),
				}); err != nil {
					p.handleOutputFailure(err)
					return
				}
			case serve.ResolvedInteraction:
				if err := p.send(map[string]any{
					"type":        "interaction.resolved",
					"runID":       runID,
					"turnID":      interaction.TurnID,
					"interaction": interaction,
				}); err != nil {
					p.handleOutputFailure(err)
					return
				}
			}
		}
	}
}

func (p *stdioProtocol) forwardEvents(ctx context.Context, runID string, events <-chan engine.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			event.Payload = runstate.PreviewEventPayload(event.Payload)
			if err := p.send(map[string]any{
				"type":  "run.event",
				"runID": runID,
				"event": event,
			}); err != nil {
				p.handleOutputFailure(err)
				return
			}
		}
	}
}
