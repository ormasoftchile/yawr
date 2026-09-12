package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
	"github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type interactionCommitCall struct {
	turnID string
	state  engine.InteractionState
}

type blockingInteractionCommitter struct {
	prepareStarted chan engine.InteractionState
	prepareRelease chan struct{}
	acceptStarted  chan interactionCommitCall
	acceptRelease  chan struct{}
	restored       *engine.InteractionState
}

func (committer *blockingInteractionCommitter) PrepareInteraction(_ context.Context, proposed engine.InteractionState) (engine.InteractionState, error) {
	committer.prepareStarted <- proposed
	if committer.prepareRelease != nil {
		<-committer.prepareRelease
	}
	if committer.restored != nil {
		return *committer.restored, nil
	}
	return proposed, nil
}

func (committer *blockingInteractionCommitter) AcceptInteraction(
	_ context.Context,
	turnID string,
	answerDigest string,
	answer json.RawMessage,
) (engine.InteractionState, error) {
	state := engine.InteractionState{
		TurnID: turnID, Status: engine.InteractionStatusAnswered,
		AnswerDigest: answerDigest, Answer: append(json.RawMessage(nil), answer...),
	}
	committer.acceptStarted <- interactionCommitCall{turnID: turnID, state: state}
	if committer.acceptRelease != nil {
		<-committer.acceptRelease
	}
	return state, nil
}

func TestPromptBrokerCommitsPendingBeforePublishing(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-durable-pending"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	frames, err := broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	committer := &blockingInteractionCommitter{
		prepareStarted: make(chan engine.InteractionState, 1), prepareRelease: make(chan struct{}),
		acceptStarted: make(chan interactionCommitCall, 1),
	}
	ctx := engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer)
	done := make(chan error, 1)
	go func() {
		_, promptErr := broker.PromptChoice(ctx, input.ChoiceRequest{
			StepID: "choose", Prompt: "Choose", Options: []input.Option{{Label: "One", Value: "one"}},
		})
		done <- promptErr
	}()
	prepared := <-committer.prepareStarted
	if prepared.Status != engine.InteractionStatusPending || prepared.StepID != "choose" || prepared.RequestDigest == "" {
		t.Fatalf("prepared interaction = %#v", prepared)
	}
	select {
	case frame := <-frames:
		t.Fatalf("interaction published before durable prepare: %#v", frame)
	default:
	}
	close(committer.prepareRelease)
	var pending PendingInteraction
	select {
	case frame := <-frames:
		var ok bool
		pending, ok = frame.(PendingInteraction)
		if !ok {
			t.Fatalf("frame = %T, want PendingInteraction", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("pending interaction was not published after commit")
	}
	if err := broker.Answer(runID, pending.TurnID, AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("PromptChoice: %v", err)
	}
}

func TestPromptBrokerCommitsAnswerBeforeDelivery(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-durable-answer"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	frames, _ := broker.Subscribe(runID, 0)
	committer := &blockingInteractionCommitter{
		prepareStarted: make(chan engine.InteractionState, 1),
		acceptStarted:  make(chan interactionCommitCall, 1), acceptRelease: make(chan struct{}),
	}
	ctx := engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer)
	response := make(chan *input.ChoiceResponse, 1)
	go func() {
		result, _ := broker.PromptChoice(ctx, input.ChoiceRequest{
			StepID: "choose", Prompt: "Choose", Options: []input.Option{{Label: "One", Value: "one"}},
		})
		response <- result
	}()
	<-committer.prepareStarted
	pending := (<-frames).(PendingInteraction)
	answerDone := make(chan error, 1)
	go func() {
		answerDone <- broker.Answer(runID, pending.TurnID, AnswerEnvelope{Kind: "choice", Selected: []string{"o:0"}})
	}()
	accepted := <-committer.acceptStarted
	if accepted.turnID != pending.TurnID || accepted.state.AnswerDigest == "" {
		t.Fatalf("accepted interaction = %#v", accepted)
	}
	select {
	case result := <-response:
		t.Fatalf("answer delivered before durable accept: %#v", result)
	default:
	}
	close(committer.acceptRelease)
	if err := <-answerDone; err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result := <-response; result == nil || len(result.Selected) != 1 || result.Selected[0] != "one" {
		t.Fatalf("response = %#v", result)
	}
}

func TestPromptBrokerReturnsRestoredAnswerWithoutRepublishing(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-restored-answer"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	frames, _ := broker.Subscribe(runID, 0)
	committer := &blockingInteractionCommitter{
		prepareStarted: make(chan engine.InteractionState, 1),
		acceptStarted:  make(chan interactionCommitCall, 1),
		restored: &engine.InteractionState{
			SchemaVersion: engine.InteractionStateSchemaV1, TurnID: "turn-restored",
			Status: engine.InteractionStatusAnswered,
			Answer: json.RawMessage(`{"kind":"choice","selected":["one"]}`),
		},
	}
	committer.restored.AnswerDigest = engine.InteractionPayloadDigest(committer.restored.Answer)
	ctx := engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer)
	result, err := broker.PromptChoice(ctx, input.ChoiceRequest{
		StepID: "choose", Prompt: "Choose", Options: []input.Option{{Label: "One", Value: "one"}},
	})
	if err != nil {
		t.Fatalf("PromptChoice: %v", err)
	}
	if len(result.Selected) != 1 || result.Selected[0] != "one" {
		t.Fatalf("restored response = %#v", result)
	}
	select {
	case frame := <-frames:
		t.Fatalf("restored answered interaction was republished: %#v", frame)
	default:
	}
}

type ordinalAnswerCommitter struct {
	ordinals []int
}

type restoredEnvelopeCommitter struct {
	answer     json.RawMessage
	acceptedAt string
	auditToken string
}

func (committer *restoredEnvelopeCommitter) PrepareInteraction(_ context.Context, proposed engine.InteractionState) (engine.InteractionState, error) {
	proposed.Status = engine.InteractionStatusAnswered
	proposed.Answer = append(json.RawMessage(nil), committer.answer...)
	proposed.AnswerDigest = engine.InteractionPayloadDigest(proposed.Answer)
	proposed.AcceptedAt = committer.acceptedAt
	proposed.AuditToken = committer.auditToken
	return proposed, nil
}

func (*restoredEnvelopeCommitter) AcceptInteraction(context.Context, string, string, json.RawMessage) (engine.InteractionState, error) {
	return engine.InteractionState{}, errors.New("unexpected accept for restored answer")
}

func (committer *ordinalAnswerCommitter) PrepareInteraction(_ context.Context, proposed engine.InteractionState) (engine.InteractionState, error) {
	committer.ordinals = append(committer.ordinals, proposed.Ordinal)
	answer, _ := json.Marshal(AnswerEnvelope{Kind: "choice", Selected: []string{fmt.Sprintf("answer-%d", proposed.Ordinal)}})
	proposed.Status = engine.InteractionStatusAnswered
	proposed.AnswerDigest = digestInteractionJSON(answer)
	proposed.Answer = answer
	return proposed, nil
}

func (*ordinalAnswerCommitter) AcceptInteraction(context.Context, string, string, json.RawMessage) (engine.InteractionState, error) {
	return engine.InteractionState{}, errors.New("unexpected accept for restored answer")
}

func TestPromptBrokerDoesNotReuseAnswerAcrossRepeatedPromptOccurrences(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-repeated-prompt"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	committer := &ordinalAnswerCommitter{}
	tracker := engine.NewInteractionInvocationTracker()
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer),
		tracker,
	)
	request := input.ChoiceRequest{
		StepID: "choose", Prompt: "Same prompt",
		Options: []input.Option{
			{Label: "Answer one", Value: "answer-1"},
			{Label: "Answer two", Value: "answer-2"},
		},
	}
	first, err := broker.PromptChoice(ctx, request)
	if err != nil {
		t.Fatalf("first PromptChoice: %v", err)
	}
	second, err := broker.PromptChoice(ctx, request)
	if err != nil {
		t.Fatalf("second PromptChoice: %v", err)
	}
	if fmt.Sprint(first.Selected) != "[answer-1]" || fmt.Sprint(second.Selected) != "[answer-2]" {
		t.Fatalf("answers were reused: first=%v second=%v", first.Selected, second.Selected)
	}
	if fmt.Sprint(committer.ordinals) != "[1 2]" {
		t.Fatalf("interaction ordinals = %v, want [1 2]", committer.ordinals)
	}
}

func TestPromptBrokerFallsBackWhenInteractionTrackerMissing(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-missing-interaction-tracker"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	committer := &ordinalAnswerCommitter{}
	ctx := engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer)
	request := input.ChoiceRequest{
		StepID: "choose", Prompt: "Choose",
		Options: []input.Option{
			{Label: "Answer one", Value: "answer-1"},
			{Label: "Answer two", Value: "answer-2"},
		},
	}
	first, err := broker.PromptChoice(ctx, request)
	if err != nil {
		t.Fatalf("first PromptChoice: %v", err)
	}
	second, err := broker.PromptChoice(ctx, request)
	if err != nil {
		t.Fatalf("second PromptChoice: %v", err)
	}
	if fmt.Sprint(first.Selected) != "[answer-1]" || fmt.Sprint(second.Selected) != "[answer-2]" ||
		fmt.Sprint(committer.ordinals) != "[1 2]" {
		t.Fatalf("first=%v second=%v ordinals=%v", first.Selected, second.Selected, committer.ordinals)
	}
}

func TestPromptBrokerRejectsMalformedRestoredAnswers(t *testing.T) {
	tests := []struct {
		name       string
		answer     string
		acceptedAt string
		auditToken string
		invoke     func(*PromptBroker, context.Context) error
	}{
		{
			name: "choice", answer: `{"kind":"choice","selected":["not-declared"]}`,
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.PromptChoice(ctx, input.ChoiceRequest{
					StepID: "choose", Options: []input.Option{{Label: "One", Value: "one"}},
				})
				return err
			},
		},
		{
			name: "decision", answer: `{"kind":"decision","label":"not-declared"}`,
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.PromptDecision(ctx, input.DecisionRequest{
					StepID: "decide", Routes: []input.Route{{Label: "known"}},
				})
				return err
			},
		},
		{
			name: "collector", answer: `{"kind":"collector","values":{}}`,
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.PromptForm(ctx, input.FormRequest{
					StepID: "collect", Fields: []input.FormField{{Name: "required", Type: "text", Required: true}},
				})
				return err
			},
		},
		{
			name: "approval", answer: `{"kind":"approval","approved":true}`,
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.RequestApproval(ctx, "approve", "verify")
				return err
			},
		},
		{
			name: "approval timestamp", answer: `{"kind":"approval","approved":true,"approver":"operator-id"}`,
			acceptedAt: "not-a-timestamp", auditToken: "approval-token",
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.RequestApproval(ctx, "approve", "verify")
				return err
			},
		},
		{
			name: "approval token", answer: `{"kind":"approval","approved":true,"approver":"operator-id"}`,
			acceptedAt: "2026-08-31T06:00:00Z",
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				_, err := broker.RequestApproval(ctx, "approve", "verify")
				return err
			},
		},
		{
			name: "host action", answer: `{"kind":"host_action","status":"completed"}`,
			invoke: func(broker *PromptBroker, ctx context.Context) error {
				ctx = hostaction.WithStepID(ctx, "open")
				_, err := broker.ExecuteHostAction(ctx, hostaction.Request{
					Capability: "product.open", Payload: map[string]any{"id": "safe"},
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broker := NewPromptBroker(8)
			const runID = "run-malformed-restored-answer"
			broker.Register(runID)
			t.Cleanup(func() { broker.Unregister(runID) })
			committer := &restoredEnvelopeCommitter{
				answer: json.RawMessage(test.answer), acceptedAt: test.acceptedAt, auditToken: test.auditToken,
			}
			ctx := engine.WithInteractionInvocationTracker(
				engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer),
				engine.NewInteractionInvocationTracker(),
			)
			if err := test.invoke(broker, ctx); err == nil {
				t.Fatal("malformed restored answer was accepted")
			}
		})
	}
}

func TestPromptBrokerRestoresApprovalAuditMetadata(t *testing.T) {
	broker := NewPromptBroker(8)
	const runID = "run-restored-approval"
	broker.Register(runID)
	t.Cleanup(func() { broker.Unregister(runID) })
	answer := json.RawMessage(`{"kind":"approval","approved":true,"approver":"operator-id"}`)
	committer := &blockingInteractionCommitter{
		prepareStarted: make(chan engine.InteractionState, 1),
		acceptStarted:  make(chan interactionCommitCall, 1),
		restored: &engine.InteractionState{
			SchemaVersion: engine.InteractionStateSchemaV1, TurnID: "turn-original",
			Status:       engine.InteractionStatusAnswered,
			AnswerDigest: engine.InteractionPayloadDigest(answer), Answer: answer,
			AcceptedAt: "2026-08-30T23:59:00Z", AuditToken: "approval-audit-token",
		},
	}
	ctx := engine.WithInteractionInvocationTracker(
		engine.WithInteractionCommitter(engine.WithRunID(context.Background(), runID), committer),
		engine.NewInteractionInvocationTracker(),
	)
	record, err := broker.RequestApproval(ctx, "approve", "Confirm")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if record.Approver != "operator-id" || record.ApprovedAt != "2026-08-30T23:59:00Z" || record.Token != "approval-audit-token" {
		t.Fatalf("restored approval record changed: %#v", record)
	}
}

func TestApprovalAnswerRequiresExplicitApprovedField(t *testing.T) {
	frame := PendingInteraction{Kind: "approval"}
	var omitted AnswerEnvelope
	if err := json.Unmarshal([]byte(`{"kind":"approval"}`), &omitted); err != nil {
		t.Fatalf("decode omitted approval: %v", err)
	}
	if _, err := mapPreviewAnswerTokens(frame, omitted); !errors.Is(err, errInvalidApprovalAnswer) {
		t.Fatalf("omitted approved error = %v, want errInvalidApprovalAnswer", err)
	}

	var denied AnswerEnvelope
	if err := json.Unmarshal([]byte(`{"kind":"approval","approved":false}`), &denied); err != nil {
		t.Fatalf("decode explicit denial: %v", err)
	}
	mapped, err := mapPreviewAnswerTokens(frame, denied)
	if err != nil {
		t.Fatalf("explicit denial: %v", err)
	}
	if mapped.Approved == nil || *mapped.Approved {
		t.Fatalf("mapped denial = %#v", mapped.Approved)
	}
}

// TestInteractions_CollectorRoundTrip drives a full prompt cycle:
//  1. POST /runs starts a run.
//  2. The fake engine's Start launches a goroutine that calls
//     broker.PromptForm with engine.WithRunID(ctx, runID) — exactly what
//     the real engine does.
//  3. The test client subscribes to SSE, receives the pending frame,
//     posts an answer, and waits for the goroutine to return with the
//     decoded values.
func TestInteractions_CollectorRoundTrip(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	// 1. Create the run.
	body := strings.NewReader(`{"runbookPath":"runbook.yaml"}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 201; body=%s", resp.StatusCode, raw)
	}
	var created struct{ RunID string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.RunID == "" {
		t.Fatal("empty runID")
	}

	// 2. Tell the fake engine to call PromptForm. We do this only after
	// the run is registered so the broker queue exists.
	formResp := h.startCollectorPrompt(t, created.RunID, input.FormRequest{
		StepID: "collect_findings",
		Prompt: "Record findings",
		Fields: []input.FormField{
			{Name: "severity", Type: "choice", Label: "Severity", Required: true,
				Options: []input.Option{{Label: "Low", Value: "low"}, {Label: "High", Value: "high"}}},
			{Name: "notes", Type: "text", Label: "Notes", Ephemeral: true,
				Validation: &input.FormValidation{Pattern: "^[a-z]+$"}},
		},
	})

	// 3. Subscribe to SSE.
	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs/"+created.RunID+"/interactions", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", sseResp.StatusCode)
	}

	br := bufio.NewReader(sseResp.Body)
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame["type"] != "pending" {
		t.Fatalf("first frame type: got %v, want pending", frame["type"])
	}
	if frame["kind"] != "collector" {
		t.Fatalf("kind: got %v, want collector", frame["kind"])
	}
	if frame["stepID"] != "collect_findings" {
		t.Fatalf("stepID: got %v", frame["stepID"])
	}
	turnID, _ := frame["turnID"].(string)
	if turnID == "" {
		t.Fatal("missing turnID")
	}

	// 4. POST the answer using preview tokens from the pending frame.
	fields := frame["fields"].([]any)
	severity := fields[0].(map[string]any)
	notes := fields[1].(map[string]any)
	if notes["ephemeral"] != true {
		t.Fatalf("collector field lost ephemeral metadata: %#v", notes)
	}
	validation, ok := notes["validation"].(map[string]any)
	if !ok || validation["pattern"] != "^[a-z]+$" {
		t.Fatalf("collector field lost validation metadata: %#v", notes)
	}
	severityToken := severity["name"].(string)
	notesToken := notes["name"].(string)
	severityOptions := severity["options"].([]any)
	highToken := severityOptions[1].(map[string]any)["value"].(string)
	ans := bytes.NewBufferString(fmt.Sprintf(`{"kind":"collector","values":{%q:%q,%q:"abc"}}`, severityToken, highToken, notesToken))
	answerResp, err := http.Post(ts.URL+"/runs/"+created.RunID+"/interactions/"+turnID, "application/json", ans)
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(answerResp.Body)
		t.Fatalf("answer status: got %d, want 204; body=%s", answerResp.StatusCode, raw)
	}

	// 5. The PromptForm goroutine should now have received the answer.
	select {
	case got := <-formResp:
		if got.err != nil {
			t.Fatalf("PromptForm err: %v", got.err)
		}
		if got.resp == nil || got.resp.Values["severity"] != "high" || got.resp.Values["notes"] != "abc" {
			t.Fatalf("PromptForm values: %#v", got.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptForm did not return")
	}

	// 6. SSE should also have emitted a "resolved" frame.
	resolved, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read resolved frame: %v", err)
	}
	if resolved["type"] != "resolved" || resolved["turnID"] != turnID {
		t.Fatalf("resolved frame: %#v", resolved)
	}
}

func TestInteractions_PendingFrameIsPreviewBoundedWithoutBreakingAnswer(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runs", "application/json", strings.NewReader(`{"runbookPath":"runbook.yaml"}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	var created struct{ RunID string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	options := []input.Option{{Label: "OK", Value: "ok"}}
	for i := 0; i < 200; i++ {
		options = append(options, input.Option{Label: fmt.Sprintf("option-%03d-%s", i, strings.Repeat("L", 500)), Value: fmt.Sprintf("value-%03d", i), Hint: strings.Repeat("hint", 500)})
	}
	formResp := h.startCollectorPrompt(t, created.RunID, input.FormRequest{
		StepID: "collect_big",
		Prompt: strings.Repeat("prompt", 3000),
		Fields: []input.FormField{
			{Name: "choice", Type: "choice", Label: strings.Repeat("label", 500), Options: options},
			{Name: "notes", Type: "text", Label: "Notes", Default: strings.Repeat("default", 2000), Hint: strings.Repeat("hint", 1000)},
		},
	})

	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs/"+created.RunID+"/interactions", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	defer sseResp.Body.Close()
	br := bufio.NewReader(sseResp.Body)
	raw, err := readRawSSEFrame(br, 5*time.Second)
	if err != nil {
		t.Fatalf("read raw pending frame: %v", err)
	}
	if len(raw.data) > 18000 {
		t.Fatalf("pending interaction frame is unbounded: got %d", len(raw.data))
	}
	if !strings.Contains(raw.data, "preview_truncated") || !strings.Contains(raw.data, "options_preview_truncated") || strings.Contains(raw.data, strings.Repeat("prompt", 1000)) {
		t.Fatalf("pending frame missing truncation marker or includes raw prompt: %s", raw.data)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(raw.data), &frame); err != nil {
		t.Fatalf("decode pending frame: %v", err)
	}
	turnID, _ := frame["turnID"].(string)
	if turnID == "" || frame["stepID"] != "collect_big" {
		t.Fatalf("stable identifiers missing from pending frame: %#v", frame)
	}

	fields := frame["fields"].([]any)
	choiceField := fields[0].(map[string]any)
	choiceToken := choiceField["name"].(string)
	choiceOptions := choiceField["options"].([]any)
	okToken := choiceOptions[0].(map[string]any)["value"].(string)
	ans := bytes.NewBufferString(fmt.Sprintf(`{"kind":"collector","values":{%q:%q}}`, choiceToken, okToken))
	answerResp, err := http.Post(ts.URL+"/runs/"+created.RunID+"/interactions/"+turnID, "application/json", ans)
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusNoContent {
		rawBody, _ := io.ReadAll(answerResp.Body)
		t.Fatalf("answer status: got %d, want 204; body=%s", answerResp.StatusCode, rawBody)
	}
	select {
	case got := <-formResp:
		if got.err != nil {
			t.Fatalf("PromptForm err: %v", got.err)
		}
		if got.resp == nil || got.resp.Values["choice"] != "ok" {
			t.Fatalf("PromptForm values: %#v", got.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptForm did not return")
	}
}

func TestInteractions_PreviewTokensPreserveOversizedChoiceValue(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	original := "value-" + strings.Repeat("x", 1000)
	choiceResp := h.startChoicePrompt(t, runID, input.ChoiceRequest{StepID: "choose", Prompt: "pick", Options: []input.Option{{Label: strings.Repeat("Label", 1000), Value: original}}})
	br, closeSSE := openInteractionStream(t, ts.URL, runID)
	defer closeSSE()
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	option := frame["options"].([]any)[0].(map[string]any)
	token := option["value"].(string)
	if token == original || len(token) > 32 || len(option["label"].(string)) > 120 {
		t.Fatalf("choice value/display not tokenized and bounded: %#v", option)
	}
	postInteractionAnswer(t, ts.URL, runID, frame["turnID"].(string), fmt.Sprintf(`{"kind":"choice","selected":[%q]}`, token), http.StatusNoContent)
	select {
	case got := <-choiceResp:
		if got.err != nil || len(got.resp.Selected) != 1 || got.resp.Selected[0] != original {
			t.Fatalf("choice response did not preserve original value: resp=%#v err=%v", got.resp, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptChoice did not return")
	}
}

func TestInteractions_PreviewTokensPreserveOversizedDecisionLabel(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	original := "route-" + strings.Repeat("r", 1000)
	decisionResp := h.startDecisionPrompt(t, runID, input.DecisionRequest{StepID: "decide", Prompt: "route", Routes: []input.Route{{Label: original, Hint: strings.Repeat("hint", 1000)}}})
	br, closeSSE := openInteractionStream(t, ts.URL, runID)
	defer closeSSE()
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	route := frame["routes"].([]any)[0].(map[string]any)
	token := route["label"].(string)
	if token == original || len(token) > 32 || len(route["display_label"].(string)) > 120 {
		t.Fatalf("decision route not tokenized and bounded: %#v", route)
	}
	postInteractionAnswer(t, ts.URL, runID, frame["turnID"].(string), fmt.Sprintf(`{"kind":"decision","label":%q}`, token), http.StatusNoContent)
	select {
	case got := <-decisionResp:
		if got.err != nil || got.resp.Label != original {
			t.Fatalf("decision response did not preserve original label: resp=%#v err=%v", got.resp, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptDecision did not return")
	}
}

func TestInteractions_PreviewTokensPreserveCollectorFieldAndOptionValues(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	fieldName := "field-" + strings.Repeat("n", 1000)
	optionValue := "option-" + strings.Repeat("v", 1000)
	formResp := h.startCollectorPrompt(t, runID, input.FormRequest{StepID: "collect", Prompt: "collect", Fields: []input.FormField{{Name: fieldName, Type: "choice", Label: "Field", Options: []input.Option{{Label: "Option", Value: optionValue}}}}})
	br, closeSSE := openInteractionStream(t, ts.URL, runID)
	defer closeSSE()
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	field := frame["fields"].([]any)[0].(map[string]any)
	fieldToken := field["name"].(string)
	optionToken := field["options"].([]any)[0].(map[string]any)["value"].(string)
	if fieldToken == fieldName || len(fieldToken) > 32 || len(field["display_name"].(string)) > 120 || optionToken == optionValue || len(optionToken) > 32 {
		t.Fatalf("collector identifiers not tokenized/bounded: field=%#v", field)
	}
	postInteractionAnswer(t, ts.URL, runID, frame["turnID"].(string), fmt.Sprintf(`{"kind":"collector","values":{%q:%q}}`, fieldToken, optionToken), http.StatusNoContent)
	select {
	case got := <-formResp:
		if got.err != nil || got.resp.Values[fieldName] != optionValue {
			t.Fatalf("collector response did not preserve original key/value: resp=%#v err=%v", got.resp, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptForm did not return")
	}
}

func TestInteractions_InvalidPreviewTokenRejected(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	_ = h.startChoicePrompt(t, runID, input.ChoiceRequest{StepID: "choose", Prompt: "pick", Options: []input.Option{{Label: "A", Value: "a"}}})
	br, closeSSE := openInteractionStream(t, ts.URL, runID)
	defer closeSSE()
	frame, err := readInteractionFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	postInteractionAnswer(t, ts.URL, runID, frame["turnID"].(string), `{"kind":"choice","selected":["not-a-token"]}`, http.StatusBadRequest)
}

// TestInteractions_KindMismatch verifies the broker rejects an answer
// whose kind does not match the pending turn.
func TestInteractions_KindMismatch(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	body := strings.NewReader(`{"runbookPath":"runbook.yaml"}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	var created struct{ RunID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)

	_ = h.startCollectorPrompt(t, created.RunID, input.FormRequest{StepID: "x", Prompt: "p"})

	sseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs/"+created.RunID+"/interactions", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	defer sseResp.Body.Close()
	br := bufio.NewReader(sseResp.Body)
	frame, _ := readInteractionFrame(br)
	turnID := frame["turnID"].(string)

	ans := bytes.NewBufferString(`{"kind":"choice","selected":["a"]}`)
	answerResp, err := http.Post(ts.URL+"/runs/"+created.RunID+"/interactions/"+turnID, "application/json", ans)
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", answerResp.StatusCode)
	}
}

// TestInteractions_TerminalFailureClearsPendingPrompt verifies that a run
// failure tears down any open interaction turn. A bridge/tool failure must not
// leave the UI replaying a stale pending prompt forever.
func TestInteractions_TerminalFailureClearsPendingPrompt(t *testing.T) {
	h := newInteractionHarness(t)
	runID := "run-terminal-failure"
	h.handle.id = runID
	h.handle.state.RunID = runID
	h.server.broker.Register(runID)
	entry := &RunEntry{
		ID:          runID,
		Handle:      h.handle,
		State:       engine.RunStatusRunning,
		RunbookPath: "runbook.yaml",
	}
	h.server.registry.Add(entry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.server.wg.Add(1)
	go h.server.pumpEvents(ctx, entry)
	defer func() {
		h.handle.closeEvents()
		h.server.wg.Wait()
	}()

	formResp := h.startCollectorPrompt(t, runID, input.FormRequest{
		StepID: "collect_before_tool",
		Prompt: "Pending prompt before tool failure",
	})

	ch, err := h.server.broker.Subscribe(runID, 0)
	if err != nil {
		t.Fatalf("Subscribe before failure: %v", err)
	}
	defer h.server.broker.Unsubscribe(runID, ch)
	first := <-ch
	pending, ok := first.(PendingInteraction)
	if !ok {
		t.Fatalf("first interaction: got %T, want PendingInteraction", first)
	}

	h.handle.mu.Lock()
	h.handle.state.Status = engine.RunStatusFailed
	h.handle.mu.Unlock()
	h.handle.pushEvent(engine.Event{
		Kind:  "run/failed",
		RunID: runID,
		Payload: map[string]any{
			"run_id": runID,
			"error":  "vscode-mcp: bridge error [result_normalization_error]",
		},
	})

	select {
	case got, ok := <-ch:
		if !ok {
			// Channel closure is acceptable only after a resolved frame was published
			// to other subscribers, but this subscriber must not be left with stale pending.
			break
		}
		resolved, ok := got.(ResolvedInteraction)
		if !ok {
			t.Fatalf("after run/failed: got %T, want ResolvedInteraction or closed channel", got)
		}
		if resolved.TurnID != pending.TurnID || !resolved.Cancelled {
			t.Fatalf("resolved frame: %#v, pending turn %s", resolved, pending.TurnID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal run failure left pending interaction unresolved")
	}

	select {
	case got := <-formResp:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("PromptForm err: got %v, want context.Canceled", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PromptForm did not unblock after terminal run failure")
	}

	if _, err := h.server.broker.Subscribe(runID, 0); !errors.Is(err, errRunNotRegistered) {
		t.Fatalf("Subscribe after failure: got %v, want errRunNotRegistered", err)
	}
}

// TestInteractions_UnknownTurn returns 409 for an unknown turn id.
func TestInteractions_UnknownTurn(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	body := strings.NewReader(`{"runbookPath":"runbook.yaml"}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	var created struct{ RunID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)

	ans := bytes.NewBufferString(`{"kind":"collector","values":{}}`)
	answerResp, err := http.Post(ts.URL+"/runs/"+created.RunID+"/interactions/no-such-turn", "application/json", ans)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", answerResp.StatusCode)
	}
}

// TestInteractions_RunsCreate_BadInputs returns 400 for empty runbookPath.
func TestInteractions_RunsCreate_BadInputs(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()

	body := strings.NewReader(`{"runbookPath":""}`)
	resp, err := http.Post(ts.URL+"/runs", "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

// --- helpers ---

type promptFormResult struct {
	resp *input.FormResponse
	err  error
}

type promptChoiceResult struct {
	resp *input.ChoiceResponse
	err  error
}

type promptDecisionResult struct {
	resp *input.DecisionResponse
	err  error
}

type interactionHarness struct {
	*testServerHarness
}

func newInteractionHarness(t *testing.T) *interactionHarness {
	t.Helper()
	return &interactionHarness{newTestServerHarness(t)}
}

// startCollectorPrompt schedules a goroutine that, on the *next* call to
// the fake engine's Start, will invoke broker.PromptForm against the run
// id with engine.WithRunID(ctx, runID) attached. The result is sent on
// the returned channel.
//
// We need this two-step dance because the broker queue is registered
// inside POST /runs (after which we know the run id) but the fakeEngine's
// Start runs before that. The harness wraps Start so the goroutine fires
// against the run id we observed.
func (h *interactionHarness) startCollectorPrompt(t *testing.T, runID string, req input.FormRequest) chan promptFormResult {
	t.Helper()
	out := make(chan promptFormResult, 1)
	// Give the broker queue a moment to be registered (handleRunsCreate
	// registers it before engine.Start, so by the time we get the runID
	// back, the queue exists).
	go func() {
		ctx := engine.WithRunID(context.Background(), runID)
		resp, err := h.server.broker.PromptForm(ctx, req)
		out <- promptFormResult{resp: resp, err: err}
	}()
	return out
}

func (h *interactionHarness) startChoicePrompt(t *testing.T, runID string, req input.ChoiceRequest) chan promptChoiceResult {
	t.Helper()
	out := make(chan promptChoiceResult, 1)
	go func() {
		ctx := engine.WithRunID(context.Background(), runID)
		resp, err := h.server.broker.PromptChoice(ctx, req)
		out <- promptChoiceResult{resp: resp, err: err}
	}()
	return out
}

func (h *interactionHarness) startDecisionPrompt(t *testing.T, runID string, req input.DecisionRequest) chan promptDecisionResult {
	t.Helper()
	out := make(chan promptDecisionResult, 1)
	go func() {
		ctx := engine.WithRunID(context.Background(), runID)
		resp, err := h.server.broker.PromptDecision(ctx, req)
		out <- promptDecisionResult{resp: resp, err: err}
	}()
	return out
}

func createInteractionRun(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Post(baseURL+"/runs", "application/json", strings.NewReader(`{"runbookPath":"runbook.yaml"}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 201; body=%s", resp.StatusCode, raw)
	}
	var created struct{ RunID string }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return created.RunID
}

func openInteractionStream(t *testing.T, baseURL, runID string) (*bufio.Reader, func()) {
	t.Helper()
	sseReq, _ := http.NewRequest(http.MethodGet, baseURL+"/runs/"+runID+"/interactions", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE: %v", err)
	}
	if sseResp.StatusCode != http.StatusOK {
		sseResp.Body.Close()
		t.Fatalf("SSE status: got %d, want 200", sseResp.StatusCode)
	}
	return bufio.NewReader(sseResp.Body), func() { sseResp.Body.Close() }
}

func postInteractionAnswer(t *testing.T, baseURL, runID, turnID, body string, wantStatus int) {
	t.Helper()
	answerResp, err := http.Post(baseURL+"/runs/"+runID+"/interactions/"+turnID, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	defer answerResp.Body.Close()
	if answerResp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(answerResp.Body)
		t.Fatalf("answer status: got %d, want %d; body=%s", answerResp.StatusCode, wantStatus, raw)
	}
}

// readInteractionFrame reads a single SSE frame from an
// /runs/{id}/interactions stream and returns the JSON payload as a map.
func readInteractionFrame(br *bufio.Reader) (map[string]any, error) {
	var dataLines []string
	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, io.EOF
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &out); err != nil {
		return nil, err
	}
	return out, nil
}
