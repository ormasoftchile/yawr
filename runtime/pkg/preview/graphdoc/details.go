package graphdoc

import (
	"regexp"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
)

// StepDetails is the closed, operator-facing definition payload for one graph
// node. Kind discriminates which optional fields are meaningful.
type StepDetails struct {
	Assign                 []schema.Assignment                  `json:"assign,omitempty"`
	Role                   string                               `json:"role,omitempty"`
	ExpressionPresentation *presentation.ExpressionPresentation `json:"expression_presentation,omitempty"`
	CodePresentation       *presentation.Envelope               `json:"code_presentation,omitempty"`
	Kind                   string                               `json:"kind"`
	Common                 *CommonStepDetails                   `json:"common,omitempty"`
	Command                string                               `json:"command,omitempty"`
	Args                   []string                             `json:"args,omitempty"`
	Script                 any                                  `json:"script,omitempty"`
	Shell                  string                               `json:"shell,omitempty"`
	Workdir                string                               `json:"workdir,omitempty"`
	EnvNames               []string                             `json:"env_names,omitempty"`
	Stdin                  bool                                 `json:"stdin,omitempty"`
	Tool                   string                               `json:"tool,omitempty"`
	Action                 string                               `json:"action,omitempty"`
	Version                string                               `json:"version,omitempty"`
	Arguments              []NamedDetailValue                   `json:"arguments,omitempty"`
	Reference              string                               `json:"reference,omitempty"`
	Dynamic                bool                                 `json:"dynamic,omitempty"`
	ResolveFrom            string                               `json:"resolve_from,omitempty"`
	OnNotFound             string                               `json:"on_not_found,omitempty"`
	Expand                 string                               `json:"expand,omitempty"`
	Bindings               []NamedDetailValue                   `json:"bindings,omitempty"`
	StopIf                 []string                             `json:"stop_if,omitempty"`
	Prompt                 string                               `json:"prompt,omitempty"`
	Variable               string                               `json:"variable,omitempty"`
	Default                any                                  `json:"default,omitempty"`
	Multiple               bool                                 `json:"multiple,omitempty"`
	Min                    int                                  `json:"min,omitempty"`
	Max                    int                                  `json:"max,omitempty"`
	Options                []OptionDetails                      `json:"options,omitempty"`
	Routes                 []RouteDetails                       `json:"routes,omitempty"`
	Fields                 []CollectorFieldDetails              `json:"fields,omitempty"`
	Capability             string                               `json:"capability,omitempty"`
	Request                []NamedDetailValue                   `json:"request,omitempty"`
	Arms                   []BranchArmDetails                   `json:"arms,omitempty"`
	Roles                  []string                             `json:"roles,omitempty"`
	Pool                   []string                             `json:"pool,omitempty"`
	Required               int                                  `json:"required,omitempty"`
	Timeout                string                               `json:"timeout,omitempty"`
	OnTimeout              string                               `json:"on_timeout,omitempty"`
	Timezone               string                               `json:"timezone,omitempty"`
	BusinessCalendar       string                               `json:"business_calendar,omitempty"`
	EscalateTo             []string                             `json:"escalate_to,omitempty"`
	Assertions             []AssertionDetails                   `json:"assertions,omitempty"`
	Source                 string                               `json:"source,omitempty"`
	EventID                string                               `json:"event_id,omitempty"`
	Filter                 []NamedDetailValue                   `json:"filter,omitempty"`
	PayloadSchema          string                               `json:"payload_schema,omitempty"`
	Content                string                               `json:"content,omitempty"`
	Format                 string                               `json:"format,omitempty"`
	Category               string                               `json:"category,omitempty"`
	Code                   string                               `json:"code,omitempty"`
	On                     string                               `json:"on,omitempty"`
	Steps                  int                                  `json:"steps,omitempty"`
	Over                   string                               `json:"over,omitempty"`
	As                     string                               `json:"as,omitempty"`
	Until                  string                               `json:"until,omitempty"`
	Collect                []NamedDetailValue                   `json:"collect,omitempty"`
	Concurrency            int                                  `json:"concurrency,omitempty"`
	Branches               int                                  `json:"branches,omitempty"`
	BranchLabels           []string                             `json:"branch_labels,omitempty"`
	WaitFor                string                               `json:"wait_for,omitempty"`
	OnFailure              string                               `json:"on_failure,omitempty"`
}

type CommonStepDetails struct {
	Subtitle string            `json:"subtitle,omitempty"`
	When     string            `json:"when,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	Delay    string            `json:"delay,omitempty"`
	Retry    *RetryDetails     `json:"retry,omitempty"`
	OnError  string            `json:"on_error,omitempty"`
	Scope    string            `json:"scope,omitempty"`
	Exports  []string          `json:"exports,omitempty"`
	Captures []CaptureDetails  `json:"captures,omitempty"`
	Contract *ContractDetails  `json:"contract,omitempty"`
	Evidence []EvidenceDetails `json:"evidence,omitempty"`
}

type RetryDetails struct {
	Max         int    `json:"max"`
	Interval    string `json:"interval,omitempty"`
	Backoff     string `json:"backoff,omitempty"`
	MaxInterval string `json:"max_interval,omitempty"`
	Jitter      bool   `json:"jitter,omitempty"`
}

type CaptureDetails struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Default    any    `json:"default,omitempty"`
	HasDefault bool   `json:"has_default,omitempty"`
}

type ContractDetails struct {
	Effects       []string `json:"effects,omitempty"`
	Reads         []string `json:"reads,omitempty"`
	Writes        []string `json:"writes,omitempty"`
	Idempotent    bool     `json:"idempotent,omitempty"`
	Deterministic bool     `json:"deterministic,omitempty"`
}

type EvidenceDetails struct {
	Kind  string   `json:"kind"`
	Name  string   `json:"name"`
	Label string   `json:"label,omitempty"`
	Items []string `json:"items,omitempty"`
}

type NamedDetailValue struct {
	Name     string `json:"name"`
	Value    any    `json:"value,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

type OptionDetails struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}

type RouteDetails struct {
	Label   string `json:"label"`
	Hint    string `json:"hint,omitempty"`
	Goto    string `json:"goto,omitempty"`
	Runbook string `json:"runbook,omitempty"`
}

type CollectorFieldDetails struct {
	Name        string                  `json:"name"`
	Type        string                  `json:"type"`
	Label       string                  `json:"label,omitempty"`
	Required    bool                    `json:"required,omitempty"`
	When        string                  `json:"when,omitempty"`
	Hint        string                  `json:"hint,omitempty"`
	Default     any                     `json:"default,omitempty"`
	HasDefault  bool                    `json:"has_default,omitempty"`
	Validation  *FieldValidationDetails `json:"validation,omitempty"`
	Options     []OptionDetails         `json:"options,omitempty"`
	OptionsFrom *DynamicOptionsDetails  `json:"options_from,omitempty"`
	Multiple    bool                    `json:"multiple,omitempty"`
	Multiline   bool                    `json:"multiline,omitempty"`
	Ephemeral   bool                    `json:"ephemeral,omitempty"`
	FromStep    string                  `json:"from_step,omitempty"`
}

type FieldValidationDetails struct {
	MinLength *int    `json:"min_length,omitempty"`
	MaxLength *int    `json:"max_length,omitempty"`
	Pattern   string  `json:"pattern,omitempty"`
	Format    string  `json:"format,omitempty"`
	Min       any     `json:"min,omitempty"`
	Max       any     `json:"max,omitempty"`
	Step      float64 `json:"step,omitempty"`
}

type DynamicOptionsDetails struct {
	Provider string `json:"provider"`
	Field    string `json:"field"`
}

type BranchArmDetails struct {
	Label     string `json:"label,omitempty"`
	Condition string `json:"condition,omitempty"`
	Else      bool   `json:"else,omitempty"`
	Steps     int    `json:"steps"`
}

type AssertionDetails struct {
	Type     string `json:"type"`
	Subject  string `json:"subject"`
	Expected string `json:"expected,omitempty"`
	Path     string `json:"path,omitempty"`
}

// DetailsForResolvedStep projects an immutable execution-plan step into the
// same operator-facing definition payload used by Builder.
func DetailsForResolvedStep(step engine.ResolvedStep) *StepDetails {
	if iterate, ok := step.Spec.(*schema.IterateNode); ok {
		return detailsForIterate(iterate)
	}
	if parallel, ok := step.Spec.(*schema.ParallelNode); ok {
		return detailsForParallel(parallel)
	}
	authored := &schema.Step{
		ID: step.ID, Type: schema.StepType(step.Kind), Title: step.Name, Subtitle: step.Subtitle,
		When: step.When, Timeout: step.Timeout, Delay: step.Delay, Retry: step.Retry,
		Scope: step.Scope, Export: step.Export,
		Capture: step.Capture, CaptureDefaults: step.CaptureDefaults, Contract: step.Contract,
		RequiredEvidence: step.RequiredEvidence, OnError: step.OnError,
	}
	switch typed := step.Spec.(type) {
	case *schema.AssignSpec:
		authored.AssignSpec = typed
	case *schema.ResultsSpec:
		authored.ResultsSpec = typed
	case *schema.CLISpec:
		authored.CLI = typed
	case *schema.ToolCallSpec:
		authored.ToolCall = typed
	case *schema.IncludeSpec:
		authored.IncludeSpec = typed
	case *schema.ChoiceSpec:
		authored.ChoiceSpec = typed
	case *schema.DecisionSpec:
		authored.DecisionSpec = typed
	case *schema.CollectorSpec:
		authored.CollectorSpec = typed
	case *schema.HostActionSpec:
		authored.HostActionSpec = typed
	case *schema.HandoffSpec:
		authored.HandoffSpec = typed
	case *schema.BranchSpec:
		authored.BranchSpec = typed
	case *schema.ApproveSpec:
		authored.ApproveSpec = typed
	case *schema.AssertSpec:
		authored.AssertSpec = typed
	case *schema.CompensateSpec:
		authored.CompensateSpec = typed
	case *schema.WaitForEventSpec:
		authored.WaitForEventSpec = typed
	case *schema.EndSpec:
		authored.EndSpec = typed
	case *schema.NoopSpec:
		authored.NoopSpec = typed
	case *schema.DisplaySpec:
		authored.DisplaySpec = typed
	}
	return detailsForStep(authored)
}

func detailsForStep(step *schema.Step) *StepDetails {
	if step == nil {
		return nil
	}
	details := &StepDetails{Kind: string(step.Type), Common: commonDetails(step)}
	switch step.Type {
	case schema.StepTypeAssign:
		details.Role = "technical"
		if step.AssignSpec != nil {
			for _, write := range step.AssignSpec.Assign {
				write.Value = safeAuthoredValue(write.Name, write.Value)
				details.Assign = append(details.Assign, write)
			}
		}
	case schema.StepTypeResults:
		details.Role = "operator"
	case schema.StepTypeCLI:
		if spec := step.CLI; spec != nil {
			details.Command = safeText(spec.Command)
			details.Args = safeCLIArgs(spec.Args)
			details.Script = safeScriptValue(spec.Run)
			details.Shell = safeText(spec.Shell)
			details.Workdir = safeText(spec.Workdir)
			details.EnvNames = sortedStringKeys(spec.Env)
			details.Stdin = spec.Stdin != ""
		}
	case schema.StepTypeTool:
		if spec := step.ToolCall; spec != nil {
			details.Tool = safeText(spec.Tool.Name)
			details.Action = safeText(spec.Tool.Action)
			details.Version = spec.Tool.Version
			details.Arguments = namedDetailValues(spec.Tool.Args)
		}
	case schema.StepTypeInclude:
		if spec := step.IncludeSpec; spec != nil {
			include := spec.Include
			details.Reference = safeText(include.Reference())
			details.Dynamic = include.IsDynamic()
			details.ResolveFrom = include.ResolveFrom
			details.OnNotFound = include.OnNotFound
			details.Expand = include.Expand
			details.Bindings = namedStringDetails(include.With)
			details.Steps = len(spec.ResolvedSteps)
			if include.Gate != nil {
				details.StopIf = safeTexts(include.Gate.StopIf)
			}
		}
	case schema.StepTypeChoice:
		if spec := step.ChoiceSpec; spec != nil {
			details.Prompt, details.Variable, details.Default = safeText(spec.Prompt), spec.Variable, safeAuthoredValue(spec.Variable, spec.Default)
			details.Multiple, details.Min, details.Max = spec.Multiple, spec.MinSelections, spec.MaxSelections
			details.Options = optionDetails(spec.Options)
		}
	case schema.StepTypeDecision:
		if spec := step.DecisionSpec; spec != nil {
			details.Prompt, details.Variable = safeText(spec.Prompt), spec.Variable
			for _, route := range spec.Routes {
				details.Routes = append(details.Routes, RouteDetails{Label: safeText(route.Label), Hint: safeText(route.Hint), Goto: route.Goto, Runbook: safeText(route.Runbook)})
			}
		}
	case schema.StepTypeCollector:
		if spec := step.CollectorSpec; spec != nil {
			details.Prompt = safeText(spec.Prompt)
			details.Fields = collectorFieldDetails(spec.Fields)
		}
	case schema.StepTypeHostAction:
		if spec := step.HostActionSpec; spec != nil {
			details.Capability = spec.HostAction.Capability
			details.Request = namedDetailValues(spec.HostAction.Request)
		}
	case schema.StepTypeBranch:
		if spec := step.BranchSpec; spec != nil {
			for _, arm := range spec.Branches {
				details.Arms = append(details.Arms, BranchArmDetails{Label: safeText(arm.Label), Condition: safeText(arm.Condition), Else: arm.Else, Steps: len(arm.Steps)})
			}
		}
	case schema.StepTypeParallel:
		if spec := step.ParallelSpec; spec != nil {
			parallel := detailsForParallel(spec)
			details.Branches = parallel.Branches
			details.WaitFor = parallel.WaitFor
			details.OnFailure = parallel.OnFailure
		}
	case schema.StepTypeApprove:
		if spec := step.ApproveSpec; spec != nil {
			applyApprovalDetails(details, spec.Approvals)
			details.OnTimeout = spec.OnTimeout
			details.Timezone = spec.Timezone
			details.BusinessCalendar = spec.BusinessCalendar
		}
	case schema.StepTypeAssert:
		if spec := step.AssertSpec; spec != nil {
			for _, assertion := range spec.Assert {
				details.Assertions = append(details.Assertions, AssertionDetails{Type: assertion.Type, Subject: safeText(assertion.Subject), Expected: safeText(assertion.Expected), Path: safeText(assertion.Path)})
			}
		}
	case schema.StepTypeWaitForEvent:
		if spec := step.WaitForEventSpec; spec != nil {
			details.Source = string(spec.Event.Source)
			details.EventID = safeText(spec.Event.ID)
			details.Filter = namedStringDetails(spec.Event.Filter)
			details.PayloadSchema = spec.Event.PayloadSchema
			details.OnTimeout = spec.OnTimeout
		}
	case schema.StepTypeDisplay:
		if spec := step.DisplaySpec; spec != nil {
			details.Content = safeText(spec.Display.Content)
			details.Format = spec.Display.Format
		}
	case schema.StepTypeEnd:
		if spec := step.EndSpec; spec != nil && spec.Outcome != nil {
			details.Category = spec.Outcome.Category
			details.Code = spec.Outcome.Code
		}
	case schema.StepTypeCompensate:
		if spec := step.CompensateSpec; spec != nil {
			details.On = spec.Compensate.On
			details.Steps = len(spec.Compensate.Steps)
		}
	}
	details.projectExpressions()
	return details
}

func detailsForIterate(iterate *schema.IterateNode) *StepDetails {
	if iterate == nil {
		return nil
	}
	details := &StepDetails{
		Kind: "iterate", Over: safeText(iterate.Over), As: iterate.As, Max: iterate.Max,
		Until: safeText(iterate.Until), Collect: namedStringDetails(iterate.Collect),
		Concurrency: iterate.Concurrency, Steps: len(iterate.Steps),
	}
	details.projectExpressions()
	return details
}

func detailsForParallel(parallel *schema.ParallelNode) *StepDetails {
	if parallel == nil {
		return nil
	}
	details := &StepDetails{Kind: "parallel", Branches: len(parallel.Branches)}
	for _, branch := range parallel.Branches {
		details.BranchLabels = append(details.BranchLabels, safeText(branch.Label))
	}
	if parallel.Join != nil {
		details.WaitFor = parallel.Join.WaitFor
		details.OnFailure = parallel.Join.OnFailure
	}
	return details
}

func commonDetails(step *schema.Step) *CommonStepDetails {
	common := &CommonStepDetails{
		Subtitle: safeText(step.Subtitle), When: safeText(step.When), Timeout: step.Timeout, Delay: step.Delay,
		OnError: step.OnError, Scope: step.Scope,
		Exports: append([]string(nil), step.Export...),
	}
	if step.Retry != nil {
		common.Retry = &RetryDetails{Max: step.Retry.Max, Interval: step.Retry.Interval, Backoff: step.Retry.Backoff, MaxInterval: step.Retry.MaxInterval, Jitter: step.Retry.Jitter}
	}
	captureNames := make([]string, 0, len(step.Capture))
	for name := range step.Capture {
		captureNames = append(captureNames, name)
	}
	sort.Strings(captureNames)
	for _, name := range captureNames {
		value, hasDefault := step.CaptureDefaults[name]
		common.Captures = append(common.Captures, CaptureDetails{Name: name, Source: safeText(step.Capture[name]), Default: safeAuthoredValue(name, value), HasDefault: hasDefault})
	}
	if step.Contract != nil {
		common.Contract = &ContractDetails{Effects: safeTexts(step.Contract.Effects), Reads: safeTexts(step.Contract.Reads), Writes: safeTexts(step.Contract.Writes), Idempotent: step.Contract.Idempotent, Deterministic: step.Contract.Deterministic}
	}
	for _, evidence := range step.RequiredEvidence {
		common.Evidence = append(common.Evidence, EvidenceDetails{Kind: string(evidence.Kind), Name: evidence.Name, Label: safeText(evidence.Label), Items: safeTexts(evidence.Items)})
	}
	if common.Subtitle == "" && common.When == "" && common.Timeout == "" && common.Delay == "" && common.Retry == nil && common.OnError == "" && common.Scope == "" && len(common.Exports) == 0 && len(common.Captures) == 0 && common.Contract == nil && len(common.Evidence) == 0 {
		return nil
	}
	return common
}

func applyApprovalDetails(details *StepDetails, approval schema.ApprovalGate) {
	details.Roles = append([]string(nil), approval.Roles...)
	details.Pool = append([]string(nil), approval.Pool...)
	details.Required = approval.Required
	if details.Required == 0 {
		details.Required = approval.Min
	}
	details.Timeout = approval.Timeout
	details.OnTimeout = approval.OnTimeout
	details.EscalateTo = append([]string(nil), approval.EscalateTo...)
}

func optionDetails(options []schema.ChoiceOption) []OptionDetails {
	result := make([]OptionDetails, 0, len(options))
	for _, option := range options {
		result = append(result, OptionDetails{Label: safeText(option.Label), Value: safeText(option.Value), Hint: safeText(option.Hint)})
	}
	return result
}

func collectorFieldDetails(fields []schema.CollectorField) []CollectorFieldDetails {
	result := make([]CollectorFieldDetails, 0, len(fields))
	for _, field := range fields {
		detail := CollectorFieldDetails{Name: field.Name, Type: string(field.Type), Label: safeText(field.Label), Required: field.Required, When: safeText(field.When), Hint: safeText(field.Hint), Multiple: field.Multiple, Multiline: field.Multiline, Ephemeral: field.Ephemeral, FromStep: field.FromStep}
		if field.Default != nil {
			detail.Default, detail.HasDefault = safeAuthoredValue(field.Name, field.Default), true
		}
		if field.Validation != nil {
			detail.Validation = &FieldValidationDetails{MinLength: field.Validation.MinLength, MaxLength: field.Validation.MaxLength, Pattern: field.Validation.Pattern, Format: field.Validation.Format, Min: field.Validation.Min, Max: field.Validation.Max, Step: field.Validation.Step}
		}
		detail.Options = optionDetails(field.Options)
		if field.OptionsFrom != nil {
			detail.OptionsFrom = &DynamicOptionsDetails{Provider: field.OptionsFrom.Provider, Field: field.OptionsFrom.Field}
		}
		result = append(result, detail)
	}
	return result
}

func namedDetailValues(values map[string]any) []NamedDetailValue {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]NamedDetailValue, 0, len(keys))
	for _, key := range keys {
		value, redacted := safeNamedValue(key, values[key])
		result = append(result, NamedDetailValue{Name: key, Value: value, Redacted: redacted})
	}
	return result
}

func namedStringDetails(values map[string]string) []NamedDetailValue {
	converted := make(map[string]any, len(values))
	for key, value := range values {
		converted[key] = value
	}
	return namedDetailValues(converted)
}

func safeNamedValue(name string, value any) (any, bool) {
	if sensitiveDetailName(name) {
		return "<redacted>", true
	}
	return safeAuthoredValue(name, value), false
}

func safeAuthoredValue(name string, value any) any {
	if sensitiveDetailName(name) {
		return "<redacted>"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = safeAuthoredValue(key, item)
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			if sensitiveDetailName(key) {
				result[key] = "<redacted>"
			} else {
				result[key] = safeText(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = safeAuthoredValue("", item)
		}
		return result
	case []string:
		return safeTexts(typed)
	case string:
		return redactCredentialAssignments(typed)
	default:
		return value
	}
}

const credentialValuePattern = `(?:(?:bearer|basic)[ \t]+[^\s;&|"']+|"[^"\r\n]*"|'[^'\r\n]*'|[^\s;&|,}\]]+)`

var credentialFlagPattern = regexp.MustCompile(`(?i)(^|[\s;&|])(--?[a-z][a-z0-9_.-]*)([= \t]+)(` + credentialValuePattern + `)`)
var credentialAssignmentPattern = regexp.MustCompile(`(?i)(["']?)([a-z][a-z0-9_.-]*)(["']?)([ \t]*[:=][ \t]*)(` + credentialValuePattern + `)`)

func safeCLIArgs(args []string) []string {
	result := make([]string, len(args))
	redactNext := false
	for index, arg := range args {
		if redactNext {
			result[index] = "<redacted>"
			redactNext = false
			continue
		}
		result[index] = redactCredentialAssignments(arg)
		trimmed := strings.TrimLeft(arg, "-")
		if !strings.Contains(arg, "=") && sensitiveDetailName(trimmed) {
			redactNext = true
		}
	}
	return result
}

func safeScriptValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redactCredentialAssignments(typed)
	case map[string]string:
		result := make(map[string]string, len(typed))
		for key, script := range typed {
			result[key] = redactCredentialAssignments(script)
		}
		return result
	default:
		return safeAuthoredValue("script", value)
	}
}

func redactCredentialAssignments(value string) string {
	value = redactSensitiveMatches(value, credentialFlagPattern, 2, 4)
	return redactSensitiveMatches(value, credentialAssignmentPattern, 2, 5)
}

func redactSensitiveMatches(value string, pattern *regexp.Regexp, keyGroup, valueGroup int) string {
	matches := pattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 {
		return value
	}
	var result strings.Builder
	last := 0
	for _, match := range matches {
		keyStart, keyEnd := match[keyGroup*2], match[keyGroup*2+1]
		valueStart, valueEnd := match[valueGroup*2], match[valueGroup*2+1]
		if keyStart < 0 || valueStart < 0 || !sensitiveDetailName(value[keyStart:keyEnd]) {
			continue
		}
		result.WriteString(value[last:valueStart])
		result.WriteString("<redacted>")
		last = valueEnd
	}
	if last == 0 {
		return value
	}
	result.WriteString(value[last:])
	return result.String()
}

func safeText(value string) string {
	return redactCredentialAssignments(value)
}

func safeTexts(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = safeText(value)
	}
	return result
}

func sensitiveDetailName(name string) bool {
	return sensitive.Name(name)
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
