package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/hostaction"
)

const testCapability hostaction.Capability = "product.open-resource"

func genericHostRequest() hostaction.Request {
	return hostaction.Request{
		Capability: testCapability,
		Payload: map[string]any{
			"resource": "incident-42",
			"options":  map[string]any{"focus": true},
		},
	}
}

func TestHostActionInteraction_RoundTripPreservesOpaqueResult(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)

	type executionResult struct {
		response hostaction.Response
		err      error
	}
	resultCh := make(chan executionResult, 1)
	go func() {
		ctx := hostaction.WithStepID(engine.WithRunID(context.Background(), runID), "open_resource")
		response, err := h.server.broker.ExecuteHostAction(ctx, genericHostRequest())
		resultCh <- executionResult{response: response, err: err}
	}()

	stream, closeStream := openInteractionStream(t, ts.URL, runID)
	defer closeStream()
	frame, err := readInteractionFrame(stream)
	if err != nil {
		t.Fatalf("read interaction: %v", err)
	}
	if frame["kind"] != "host_action" || frame["stepID"] != "open_resource" {
		t.Fatalf("pending frame = %#v", frame)
	}
	turnID := frame["turnID"].(string)
	correlationID := frame["correlationID"].(string)
	action := frame["host_action"].(map[string]any)
	if action["capability"] != string(testCapability) {
		t.Fatalf("capability lost: %#v", action)
	}
	payload, ok := action["request"].(map[string]any)
	if !ok || payload["resource"] != "incident-42" {
		t.Fatalf("payload lost: %#v", action)
	}

	answer := `{"kind":"host_action","runID":"` + runID + `","turnID":"` + turnID +
		`","correlationID":"` + correlationID + `","capability":"product.open-resource",` +
		`"status":"completed","result":{"state":"opened","details":{"attempt":1}}}`
	postInteractionAnswer(t, ts.URL, runID, turnID, answer, http.StatusNoContent)

	select {
	case got := <-resultCh:
		if got.err != nil || got.response.Status != hostaction.StatusCompleted {
			t.Fatalf("response=%#v err=%v", got.response, got.err)
		}
		want := map[string]any{"state": "opened", "details": map[string]any{"attempt": float64(1)}}
		if !reflect.DeepEqual(got.response.Result, want) {
			t.Fatalf("result=%#v want=%#v", got.response.Result, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("host action did not resume")
	}

	postInteractionAnswer(t, ts.URL, runID, turnID, answer, http.StatusConflict)
}

func TestHostActionInteraction_RejectsNonBridgeStatus(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)

	ctx, cancel := context.WithCancel(hostaction.WithStepID(engine.WithRunID(context.Background(), runID), "open_resource"))
	defer cancel()
	go func() { _, _ = h.server.broker.ExecuteHostAction(ctx, genericHostRequest()) }()
	stream, closeStream := openInteractionStream(t, ts.URL, runID)
	defer closeStream()
	frame, err := readInteractionFrame(stream)
	if err != nil {
		t.Fatalf("read interaction: %v", err)
	}
	turnID := frame["turnID"].(string)
	correlationID := frame["correlationID"].(string)
	answer := `{"kind":"host_action","runID":"` + runID + `","turnID":"` + turnID +
		`","correlationID":"` + correlationID + `","capability":"product.open-resource","status":"opened"}`
	postInteractionAnswer(t, ts.URL, runID, turnID, answer, http.StatusBadRequest)
}

func TestHostActionInteraction_RejectsResultOnNonCompletedStatus(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	ctx, cancel := context.WithCancel(hostaction.WithStepID(engine.WithRunID(context.Background(), runID), "open_resource"))
	defer cancel()
	go func() { _, _ = h.server.broker.ExecuteHostAction(ctx, genericHostRequest()) }()
	stream, closeStream := openInteractionStream(t, ts.URL, runID)
	defer closeStream()
	frame, err := readInteractionFrame(stream)
	if err != nil {
		t.Fatalf("read interaction: %v", err)
	}
	turnID := frame["turnID"].(string)
	correlationID := frame["correlationID"].(string)
	answer := `{"kind":"host_action","runID":"` + runID + `","turnID":"` + turnID +
		`","correlationID":"` + correlationID + `","capability":"product.open-resource",` +
		`"status":"failed","result":{"state":"must-not-cross"}}`
	postInteractionAnswer(t, ts.URL, runID, turnID, answer, http.StatusBadRequest)
}

func TestHostActionInteraction_CancellationRejectsLateAcknowledgement(t *testing.T) {
	h := newInteractionHarness(t)
	ts := newHTTPTestServer(t, h.server)
	defer ts.Close()
	runID := createInteractionRun(t, ts.URL)
	ctx, cancel := context.WithCancel(hostaction.WithStepID(engine.WithRunID(context.Background(), runID), "open_resource"))
	done := make(chan error, 1)
	go func() {
		_, err := h.server.broker.ExecuteHostAction(ctx, genericHostRequest())
		done <- err
	}()
	stream, closeStream := openInteractionStream(t, ts.URL, runID)
	defer closeStream()
	frame, err := readInteractionFrame(stream)
	if err != nil {
		t.Fatalf("read interaction: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel stranded host action")
	}
	answer := `{"kind":"host_action","runID":"` + runID + `","turnID":"` + frame["turnID"].(string) +
		`","correlationID":"` + frame["correlationID"].(string) +
		`","capability":"product.open-resource","status":"completed","result":{"state":"late"}}`
	postInteractionAnswer(t, ts.URL, runID, frame["turnID"].(string), answer, http.StatusConflict)
}

func TestHostActionBroker_GenericBridgeStatusesAreAccepted(t *testing.T) {
	for _, status := range []hostaction.Status{
		hostaction.StatusCompleted,
		hostaction.StatusFailed,
		hostaction.StatusTimedOut,
		hostaction.StatusUnsupported,
		hostaction.StatusExecutionNotStarted,
	} {
		t.Run(string(status), func(t *testing.T) {
			h := newInteractionHarness(t)
			ts := newHTTPTestServer(t, h.server)
			defer ts.Close()
			runID := createInteractionRun(t, ts.URL)
			type executionResult struct {
				response hostaction.Response
				err      error
			}
			resultCh := make(chan executionResult, 1)
			go func() {
				ctx := hostaction.WithStepID(engine.WithRunID(context.Background(), runID), "open_resource")
				response, err := h.server.broker.ExecuteHostAction(ctx, genericHostRequest())
				resultCh <- executionResult{response: response, err: err}
			}()
			stream, closeStream := openInteractionStream(t, ts.URL, runID)
			defer closeStream()
			frame, err := readInteractionFrame(stream)
			if err != nil {
				t.Fatalf("read interaction: %v", err)
			}
			turnID := frame["turnID"].(string)
			correlationID := frame["correlationID"].(string)
			resultJSON := ""
			if status == hostaction.StatusCompleted {
				resultJSON = `,"result":{"state":"done"}`
			}
			answer := `{"kind":"host_action","runID":"` + runID + `","turnID":"` + turnID +
				`","correlationID":"` + correlationID + `","capability":"product.open-resource","status":"` +
				string(status) + `"` + resultJSON + `}`
			postInteractionAnswer(t, ts.URL, runID, turnID, answer, http.StatusNoContent)
			select {
			case got := <-resultCh:
				if got.err != nil || got.response.Status != status {
					t.Fatalf("response=%#v err=%v", got.response, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("host action did not resume")
			}
		})
	}
}

func TestPreviewHostActionBridgeIsGeneric(t *testing.T) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read preview: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		"yawr.host-action.request",
		"yawr.host-action.ack",
		"yawr.host-action.cancel",
		"acknowledgeHostActionCancellation",
		"data.type === 'yawr.host-action.ack' || data.type === 'yawr.host-action.cancel'",
		"request: pending.host_action.request",
		"status: data.status",
		"result: data.result",
		"ev.source !== window.parent",
		"ev.origin !== expectedOrigin",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("preview bridge missing %q", want)
		}
	}
	if strings.Contains(html, "xts.open") || strings.Contains(html, "xtsLaunchStatus") {
		t.Fatal("served preview contains product-specific XTS logic")
	}
}

func TestPreviewHostActionWireFixtureHasOneClosedCanonicalShape(t *testing.T) {
	data, err := os.ReadFile("testdata/host_action_wire_v1.json")
	if err != nil {
		t.Fatalf("read wire fixture: %v", err)
	}
	var wire struct {
		Request        map[string]any `json:"request"`
		Acknowledgment map[string]any `json:"acknowledgment"`
		Cancellation   map[string]any `json:"cancellation"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("parse wire fixture: %v", err)
	}
	request := map[string]any{
		"type": "yawr.host-action.request", "version": "yawr.host-action/v1",
		"capability": "product.open-resource",
		"runId":      "run-1", "turnId": "turn-1", "correlationId": "correlation-1",
		"previewSessionId": "preview-session-1", "requestId": "preview-session-1:correlation-1",
		"request": map[string]any{"resource": "incident-42", "options": map[string]any{"focus": true}},
	}
	ack := map[string]any{
		"type": "yawr.host-action.ack", "version": "yawr.host-action/v1",
		"runId": "run-1", "turnId": "turn-1", "correlationId": "correlation-1",
		"previewSessionId": "preview-session-1", "requestId": "preview-session-1:correlation-1",
		"capability": "product.open-resource", "status": "completed",
		"result": map[string]any{"state": "opened"}, "error": nil,
	}
	cancel := map[string]any{
		"type": "yawr.host-action.cancel", "version": "yawr.host-action/v1",
		"correlationId": "correlation-1", "previewSessionId": "preview-session-1",
		"requestId": "preview-session-1:correlation-1",
		"status":    "execution-not-started", "reason": "run-replaced",
	}
	if !reflect.DeepEqual(wire.Request, request) ||
		!reflect.DeepEqual(wire.Acknowledgment, ack) ||
		!reflect.DeepEqual(wire.Cancellation, cancel) {
		t.Fatalf("canonical wire drifted: %#v", wire)
	}
}

func TestPreviewInteractionFrameKeepsCorrelationHostActionOnly(t *testing.T) {
	choice := previewInteractionFrame(PendingInteraction{
		Type: "pending", ID: 1, RunID: "run-1", TurnID: "turn-1", StepID: "choice-1",
		Kind: "choice", CorrelationID: "must-not-leak",
	}).(map[string]any)
	if _, found := choice["correlationID"]; found {
		t.Fatalf("choice carries host correlation: %#v", choice)
	}
	host := previewInteractionFrame(PendingInteraction{
		Type: "pending", ID: 2, RunID: "run-1", TurnID: "turn-2", StepID: "host-1",
		Kind: "host_action", CorrelationID: "host-correlation",
		HostAction: &hostaction.Request{Capability: testCapability, Payload: map[string]any{}},
	}).(map[string]any)
	if host["correlationID"] != "host-correlation" {
		t.Fatalf("host action lost correlation: %#v", host)
	}
}
