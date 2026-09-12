package serve

// F-4 regression test (barbara-client-enum-parity-gate-review.md, B-3):
// the web GUI's raw-JSON-only input experience must be replaced by a
// declaration-driven input form once the graphjson DTO exists (F-1). This
// asserts the served preview.html defines the closed-selector /
// free-text form component described in AR-CE-3 §3, keeps the raw-JSON
// box as a documented fallback (never the only surface), and renders
// enum members via a closed <select> rather than free-form JS.

import (
	"strings"
	"testing"
)

func TestPreviewHTML_StaleRequestCannotUnlockNewRunFetch(t *testing.T) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	body := string(data)
	finallyStart := strings.Index(body, "} finally {")
	callbackEnd := strings.Index(body, "  }, [runbookPath, runID]);")
	if finallyStart < 0 || callbackEnd < finallyStart {
		t.Fatalf("preview.html loadDoc finally block not found")
	}
	finallyBlock := body[finallyStart:callbackEnd]
	if !strings.Contains(finallyBlock, "if (docAbortRef.current === controller) {") ||
		!strings.Contains(finallyBlock, "docFetchInFlightRef.current = false;") {
		t.Fatalf("preview.html must clear docFetchInFlightRef only inside the current-controller guard; stale old-run requests must not unlock a newer run fetch")
	}

	loader := newPreviewDocLoaderModel()
	oldReq := loader.start()
	loader.abort(oldReq)
	newReq := loader.start()
	if !loader.inFlight {
		t.Fatal("new-run request did not acquire in-flight lock")
	}
	loader.finish(oldReq)
	if !loader.inFlight {
		t.Fatal("stale old-run request released the new-run in-flight lock")
	}
	loader.finish(newReq)
	if loader.inFlight {
		t.Fatal("current new-run request did not release its lock")
	}
}

type previewDocLoaderModel struct {
	current  *previewDocRequestModel
	inFlight bool
	nextID   int
}

type previewDocRequestModel struct{ id int }

func newPreviewDocLoaderModel() *previewDocLoaderModel { return &previewDocLoaderModel{} }

func (m *previewDocLoaderModel) start() *previewDocRequestModel {
	if m.inFlight {
		return nil
	}
	m.nextID++
	req := &previewDocRequestModel{id: m.nextID}
	m.inFlight = true
	m.current = req
	return req
}

func (m *previewDocLoaderModel) abort(req *previewDocRequestModel) {
	if m.current == req {
		m.current = nil
		m.inFlight = false
	}
}

func (m *previewDocLoaderModel) finish(req *previewDocRequestModel) {
	if m.current == req {
		m.current = nil
		m.inFlight = false
	}
}

func TestPreviewHTML_TerminalPollingIsBounded(t *testing.T) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	body := string(data)

	checks := []string{
		"const PREVIEW_DOC_POLL_INTERVAL_MS = 3000",
		"TERMINAL_RUN_STATUSES",
		"isTerminalRunStatus",
		"docFetchInFlightRef.current",
		"AbortController",
		"If-None-Match",
		"res.status === 304",
		"if (!manual && isTerminalRef.current) return",
		"if (!runID || isTerminal) return",
		"clearInterval(timer)",
		"docAbortRef.current.abort()",
		"frontierRefreshKeyRef.current === key",
		"}, [runID, isTerminal]);",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Fatalf("preview.html missing polling guard %q", want)
		}
	}
	if strings.Contains(body, "if (!known.has(id)) { loadDoc(); return; }") {
		t.Fatalf("preview.html still has immediate unknown-state document refetch loop")
	}
	if strings.Contains(body, "if (pending.stepID && !known.has(pending.stepID)) loadDoc();") {
		t.Fatalf("preview.html still has immediate unknown-pending document refetch loop")
	}
}

func TestPreviewHTML_DeclarationDrivenInputsForm(t *testing.T) {
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "function InputsForm") {
		t.Fatalf("preview.html no longer defines a declaration-driven InputsForm component (B-3)")
	}
	// The raw-JSON box (T-GUI-INPUT-FORM's documented fallback, AR-CE-3
	// §3.6) must still exist -- it is a fallback, not something this
	// revision removes.
	if !strings.Contains(body, "inputs JSON (optional)") {
		t.Fatalf("preview.html no longer offers the raw-JSON fallback box (AR-CE-3 §3.6 requires it remain)")
	}
	// The form must render enum members as a closed <select>, never as
	// free-form membership-checking JS.
	if !strings.Contains(body, "d.enum") || !strings.Contains(body, "e('select'") {
		t.Fatalf("preview.html InputsForm does not render a closed selector from declared enum members")
	}
	// Redaction (C1): the form must branch on enumRedacted and must
	// never render a member list for it.
	if !strings.Contains(body, "d.enumRedacted") {
		t.Fatalf("preview.html InputsForm does not special-case enumRedacted inputs (C1)")
	}
	// The form must be wired into App's render tree (not dead code).
	if !strings.Contains(body, "e(InputsForm,") {
		t.Fatalf("InputsForm is defined but never rendered from App -- dead code, not a live surface")
	}
	// Submission must go through buildFormInputs into the real /runs
	// path, not a second submission mechanism.
	if !strings.Contains(body, "buildFormInputs") {
		t.Fatalf("preview.html does not wire the declared-input form into the real /runs submission path")
	}
}
