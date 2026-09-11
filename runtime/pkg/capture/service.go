// Package capture implements YAWR Capture Path evaluation over executor results.
package capture

import (
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	gcpparser "github.com/ormasoftchile/yawr/runtime/pkg/gcp/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/gdp"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

// Event is the event-source envelope available to event.* captures.
type Event struct {
	ID      string
	Body    string
	Headers map[string]string
}

// Service resolves capture declarations against current and prior step results.
type Service struct {
	Vars        map[string]any
	StepResults map[string]*engine.StepResult
	Event       *Event
}

// New constructs a capture service.
func New(vars map[string]any, stepResults map[string]*engine.StepResult, event *Event) *Service {
	return &Service{Vars: vars, StepResults: stepResults, Event: event}
}

// Capture evaluates the capture block for stepID from vp against stepResult.
func (s *Service) Capture(vp *engine.ValidatedPlan, stepID string, stepResult *engine.StepResult) (map[string]pjvm.Value, error) {
	step, ok := findStep(vp, stepID)
	if !ok {
		return map[string]pjvm.Value{}, nil
	}
	return s.CaptureStep(vp, step, stepResult)
}

// ResolveSource parses source as a YAWR Capture Path and resolves it
// against current (the step whose own local/outputs/http/event namespace
// applies, may be nil) and s.StepResults (for step.<id>... cross-step
// paths), returning an ordinary Go value. This is the entry point for any
// caller -- e.g. a substituted runbook's outputs: block -- that needs to
// resolve a single ad hoc GCP path string rather than a step's full
// Capture map.
func (s *Service) ResolveSource(source string, current *engine.StepResult) (any, error) {
	path, err := gcpparser.Parse(source)
	if err != nil {
		return nil, err
	}
	val, err := s.resolve(path, current)
	if err != nil {
		return nil, err
	}
	return ToAny(val), nil
}

// CaptureStep evaluates step.Capture against stepResult.
func (s *Service) CaptureStep(vp *engine.ValidatedPlan, step engine.ResolvedStep, stepResult *engine.StepResult) (map[string]pjvm.Value, error) {
	out := map[string]pjvm.Value{}
	for name, source := range step.Capture {
		path, err := parsedPath(vp, step.ID, name, source)
		if err != nil {
			return nil, fmt.Errorf("capture %q: %w", name, err)
		}
		val, err := s.resolve(path, stepResult)
		def, hasDefault := step.CaptureDefaults[name]
		if hasDefault {
			val, err = applyDefault(name, source, path, val, err, def)
		}
		if err != nil {
			return nil, fmt.Errorf("capture %q: %w", name, err)
		}
		out[name] = val
	}
	return out, nil
}

// ToAnyMap converts PJVM captures to ordinary Go values for engine variable maps.
func ToAnyMap(values map[string]pjvm.Value) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = ToAny(v)
	}
	return out
}

// ToAny converts a PJVM value to ordinary Go JSON-like values.
func ToAny(v pjvm.Value) any {
	switch v.Kind() {
	case pjvm.KindNull:
		return nil
	case pjvm.KindBool:
		b, _ := v.BoolValue()
		return b
	case pjvm.KindNumber:
		n, _ := v.NumberValue()
		if n == float64(int64(n)) {
			return int(n)
		}
		return n
	case pjvm.KindString:
		s, _ := v.StringValue()
		return s
	case pjvm.KindArray:
		arr, _ := v.ArrayValue()
		out := make([]any, len(arr))
		for i, elem := range arr {
			out[i] = ToAny(elem)
		}
		return out
	case pjvm.KindObject:
		obj, _ := v.ObjectValue()
		out := make(map[string]any, len(obj))
		for k, elem := range obj {
			out[k] = ToAny(elem)
		}
		return out
	default:
		return nil
	}
}

func findStep(vp *engine.ValidatedPlan, stepID string) (engine.ResolvedStep, bool) {
	if vp == nil || vp.Source == nil {
		return engine.ResolvedStep{}, false
	}
	for _, step := range vp.Source.Steps {
		if step.ID == stepID {
			return step, true
		}
	}
	return engine.ResolvedStep{}, false
}

func parsedPath(vp *engine.ValidatedPlan, stepID, name, source string) (*gdp.Path, error) {
	if vp != nil && vp.GCPPaths != nil {
		if path := vp.GCPPaths[engine.StepRef{StepID: stepID, FieldPath: "capture." + name}]; path != nil {
			return path, nil
		}
	}
	return gcpparser.Parse(source)
}

func (s *Service) resolve(path *gdp.Path, current *engine.StepResult) (pjvm.Value, error) {
	switch path.Source.Kind {
	case gdp.SourceLocal:
		if current == nil {
			return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "current step output is not available")
		}
		return resolveLocal(path, current.Output)
	case gdp.SourceStep:
		if s == nil || s.StepResults == nil || s.StepResults[path.Source.StepID] == nil {
			return pjvm.Null(), errkit.New("GCP-RESOLVE-001", fmt.Sprintf("step %q has not produced output", path.Source.StepID))
		}
		return resolveLocal(path, s.StepResults[path.Source.StepID].Output)
	case gdp.SourceHTTP:
		if current == nil {
			return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "http source is not available")
		}
		return resolveEnvelope(path, current.Output, "http")
	case gdp.SourceEvent:
		return s.resolveEvent(path, current)
	case gdp.SourceOutputs:
		return resolveOutputs(path, current)
	default:
		return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "capture source is not available")
	}
}

// resolveOutputs resolves outputs.<name> against the CURRENT step's own
// Output map only.
// capture namespace: no step.{id}.outputs.{name} cross-step form exists).
// current.Output[name] is populated by ToolExecutor.executeSubstitution
// exactly for a substituted (execute.kind: runbook) action's declared
// outputs: an unknown name is GCP-RESOLVE-002, matching every other
// unresolved-segment case in this file.
func resolveOutputs(path *gdp.Path, current *engine.StepResult) (pjvm.Value, error) {
	if current == nil {
		return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "current step output is not available")
	}
	if path.Source.Field == "" {
		if current.Status != engine.StepStatusCompleted || current.PublicOutputs == nil {
			return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "validated declared public outputs are unavailable")
		}
		return pjvm.FromAny(current.PublicOutputs)
	}
	if current.PublicOutputs != nil {
		if val, ok := current.PublicOutputs[path.Source.Field]; ok {
			root, err := pjvm.FromAny(val)
			if err != nil || len(path.Segments) == 0 {
				return root, err
			}
			return gdp.Resolve(root, path)
		}
	}
	val, ok := current.Output[path.Source.Field]
	if !ok {
		return pjvm.Null(), errkit.New("GCP-RESOLVE-002", fmt.Sprintf("output %q was not produced by this step", path.Source.Field))
	}
	root, err := pjvm.FromAny(val)
	if err != nil {
		return pjvm.Null(), err
	}
	if len(path.Segments) == 0 {
		return root, nil
	}
	return gdp.Resolve(root, path)
}

func resolveLocal(path *gdp.Path, output map[string]any) (pjvm.Value, error) {
	switch path.Source.Field {
	case "exit_code":
		return pjvm.FromAny(output["exit_code"])
	case "stdout", "stderr":
		text, _ := output[path.Source.Field].(string)
		if len(path.Segments) == 0 {
			return pjvm.NewString(text)
		}
		root, err := pjvm.FromJSON([]byte(text))
		if err != nil {
			return pjvm.Null(), errkit.Wrap("GCP-RESOLVE-004", path.Source.Field+" is not parseable as JSON", err)
		}
		return gdp.Resolve(root, path)
	case "json":
		return resolveStructured(path, output["stdout"], "JSON")
	case "yaml":
		return resolveStructured(path, output["stdout"], "YAML")
	default:
		return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "local source is not available")
	}
}

func resolveStructured(path *gdp.Path, raw any, format string) (pjvm.Value, error) {
	text, _ := raw.(string)
	var root pjvm.Value
	var err error
	if format == "YAML" {
		root, err = pjvm.FromYAML([]byte(text))
	} else {
		root, err = pjvm.FromJSON([]byte(text))
	}
	if err != nil {
		return pjvm.Null(), errkit.Wrap("GCP-RESOLVE-004", "stdout is not parseable as "+format, err)
	}
	if len(path.Segments) == 0 {
		return root, nil
	}
	return gdp.Resolve(root, path)
}

func resolveEnvelope(path *gdp.Path, output map[string]any, source string) (pjvm.Value, error) {
	env := envelope(output, source)
	if env == nil {
		return pjvm.Null(), errkit.New("GCP-RESOLVE-001", source+" source is not available")
	}
	switch path.Source.Field {
	case "status", "id":
		return pjvm.FromAny(env[path.Source.Field])
	case "body":
		text, _ := env["body"].(string)
		if len(path.Segments) == 0 {
			return pjvm.NewString(text)
		}
		root, err := pjvm.FromJSON([]byte(text))
		if err != nil {
			return pjvm.Null(), errkit.Wrap("GCP-RESOLVE-004", source+" body is not parseable as JSON", err)
		}
		return gdp.Resolve(root, path)
	case "headers":
		return headerValue(env["headers"], path.Source.Header)
	default:
		return pjvm.Null(), errkit.New("GCP-RESOLVE-001", source+" field is not available")
	}
}

func (s *Service) resolveEvent(path *gdp.Path, current *engine.StepResult) (pjvm.Value, error) {
	if s != nil && s.Event != nil {
		return resolveEnvelope(path, map[string]any{"event": map[string]any{"id": s.Event.ID, "body": s.Event.Body, "headers": s.Event.Headers}}, "event")
	}
	if current != nil {
		if v, err := resolveEnvelope(path, current.Output, "event"); err == nil {
			return v, nil
		}
	}
	if s != nil && s.Vars != nil {
		return resolveEnvelope(path, s.Vars, "event")
	}
	return pjvm.Null(), errkit.New("GCP-RESOLVE-001", "event source is not available")
}

func envelope(output map[string]any, source string) map[string]any {
	if output == nil {
		return nil
	}
	if env, ok := output[source].(map[string]any); ok {
		return env
	}
	if source == "http" {
		if _, ok := output["status"]; ok {
			return output
		}
	}
	if source == "event" {
		if _, ok := output["id"]; ok {
			return output
		}
	}
	return nil
}

func headerValue(raw any, name string) (pjvm.Value, error) {
	switch headers := raw.(type) {
	case map[string]any:
		for k, v := range headers {
			if strings.EqualFold(k, name) {
				return pjvm.FromAny(v)
			}
		}
	case map[string]string:
		for k, v := range headers {
			if strings.EqualFold(k, name) {
				return pjvm.NewString(v)
			}
		}
	}
	return pjvm.Null(), nil
}

func applyDefault(name, source string, path *gdp.Path, val pjvm.Value, resolveErr error, def any) (pjvm.Value, error) {
	defaultValue, err := pjvm.FromAny(def)
	if err != nil || !isScalar(defaultValue) {
		return pjvm.Null(), errkit.New("GCP-DEFAULT-SUBTREE", fmt.Sprintf("capture %q path %q declares a non-scalar default; remove capture_default and guard consumers with when", name, source))
	}
	if typeMismatch(path, defaultValue) {
		return pjvm.Null(), errkit.New("GCP-TYPE-001", fmt.Sprintf("capture %q default type %s is incompatible with %q", name, defaultValue.Kind(), source))
	}
	if resolveErr != nil {
		if isDefaultableResolve(resolveErr) {
			return defaultValue, nil
		}
		return pjvm.Null(), resolveErr
	}
	if val.Kind() == pjvm.KindObject || val.Kind() == pjvm.KindArray {
		return pjvm.Null(), errkit.New("GCP-DEFAULT-SUBTREE", fmt.Sprintf("capture %q path %q resolved to %s; remove capture_default and guard consumers with when", name, source, val.Kind()))
	}
	if val.Kind() == pjvm.KindNull {
		return defaultValue, nil
	}
	return val, nil
}

func isScalar(v pjvm.Value) bool {
	return v.Kind() == pjvm.KindNull || v.Kind() == pjvm.KindBool || v.Kind() == pjvm.KindNumber || v.Kind() == pjvm.KindString
}

func typeMismatch(path *gdp.Path, def pjvm.Value) bool {
	if path == nil {
		return false
	}
	if path.Source.Field == "exit_code" || (path.Source.Kind == gdp.SourceHTTP && path.Source.Field == "status") {
		return def.Kind() != pjvm.KindNumber && def.Kind() != pjvm.KindNull
	}
	if path.Source.Field == "id" || path.Source.Field == "headers" {
		return def.Kind() != pjvm.KindString && def.Kind() != pjvm.KindNull
	}
	return false
}

func isDefaultableResolve(err error) bool {
	code := ""
	if c, ok := err.(interface{ Code() string }); ok {
		code = c.Code()
	}
	return code == "GCP-RESOLVE-002" || code == "GCP-RESOLVE-003"
}
