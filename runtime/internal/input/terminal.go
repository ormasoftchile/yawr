package input

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// TerminalInputProvider collects user input from an io.Reader/io.Writer pair.
type TerminalInputProvider struct {
	In     io.Reader
	Out    io.Writer
	reader *bufio.Reader
}

// Ensure TerminalInputProvider implements input.PromptProvider.
var _ input.PromptProvider = (*TerminalInputProvider)(nil)

// NewTerminalInputProvider constructs a TerminalInputProvider for the given streams.
func NewTerminalInputProvider(in io.Reader, out io.Writer) *TerminalInputProvider {
	return &TerminalInputProvider{In: in, Out: out, reader: bufio.NewReader(in)}
}

// PromptChoice renders a choice prompt and reads the user's selection.
func (t *TerminalInputProvider) PromptChoice(ctx context.Context, req input.ChoiceRequest) (*input.ChoiceResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(t.Out, "%s\n", req.Prompt); err != nil {
		return nil, err
	}
	for i, opt := range req.Options {
		hint := ""
		if opt.Hint != "" {
			hint = " - " + opt.Hint
		}
		if _, err := fmt.Fprintf(t.Out, "%d) %s%s\n", i+1, opt.Label, hint); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprint(t.Out, "Selection: "); err != nil {
		return nil, err
	}

	line, err := t.inputReader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if line == "" && req.Default != "" {
		line = req.Default
	}

	selected := parseSelections(line, req.Options)
	if req.Multiple {
		return &input.ChoiceResponse{Selected: selected}, nil
	}
	if len(selected) > 1 {
		selected = selected[:1]
	}
	return &input.ChoiceResponse{Selected: selected}, nil
}

// PromptDecision renders a decision prompt and reads the chosen route label.
func (t *TerminalInputProvider) PromptDecision(ctx context.Context, req input.DecisionRequest) (*input.DecisionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(t.Out, "%s\n", req.Prompt); err != nil {
		return nil, err
	}
	for i, route := range req.Routes {
		hint := ""
		if route.Hint != "" {
			hint = " - " + route.Hint
		}
		if _, err := fmt.Fprintf(t.Out, "%d) %s%s\n", i+1, route.Label, hint); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprint(t.Out, "Selection: "); err != nil {
		return nil, err
	}

	line, err := t.inputReader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line = strings.TrimSpace(line)
	label := parseDecision(line, req.Routes)
	return &input.DecisionResponse{Label: label}, nil
}

// PromptForm renders a form prompt and reads values for each field.
func (t *TerminalInputProvider) PromptForm(ctx context.Context, req input.FormRequest) (*input.FormResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Prompt != "" {
		if _, err := fmt.Fprintf(t.Out, "%s\n", req.Prompt); err != nil {
			return nil, err
		}
	}
	values := make(map[string]any)
	for _, field := range req.Fields {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		label := field.Label
		if label == "" {
			label = field.Name
		}
		prompt := label
		if field.Hint != "" {
			prompt += " (" + field.Hint + ")"
		}
		if _, err := fmt.Fprintf(t.Out, "%s: ", prompt); err != nil {
			return nil, err
		}
		line, err := t.inputReader().ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" && field.Default != nil {
			values[field.Name] = field.Default
			continue
		}
		values[field.Name] = coerceTerminalFormValue(field, line)
	}
	return &input.FormResponse{Values: values}, nil
}

func (t *TerminalInputProvider) inputReader() *bufio.Reader {
	if t.reader == nil {
		t.reader = bufio.NewReader(t.In)
	}
	return t.reader
}

func coerceTerminalFormValue(field input.FormField, value string) any {
	if field.Multiple {
		parts := strings.Split(value, ",")
		values := make([]any, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				values = append(values, coerceTerminalScalar(field.Type, part))
			}
		}
		return values
	}
	return coerceTerminalScalar(field.Type, value)
}

func coerceTerminalScalar(fieldType, value string) any {
	switch fieldType {
	case "boolean":
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	case "number":
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	case "integer":
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return value
}

func parseSelections(line string, options []input.Option) []string {
	if line == "" {
		return nil
	}
	parts := strings.Split(line, ",")
	var selected []string
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if idx, err := strconv.Atoi(token); err == nil {
			if idx >= 1 && idx <= len(options) {
				selected = append(selected, options[idx-1].Value)
				continue
			}
		}
		for _, opt := range options {
			if token == opt.Value || token == opt.Label {
				selected = append(selected, opt.Value)
				break
			}
		}
	}
	return selected
}

func parseDecision(line string, routes []input.Route) string {
	if line == "" {
		if len(routes) > 0 {
			return routes[0].Label
		}
		return ""
	}
	if idx, err := strconv.Atoi(line); err == nil {
		if idx >= 1 && idx <= len(routes) {
			return routes[idx-1].Label
		}
	}
	for _, route := range routes {
		if line == route.Label {
			return route.Label
		}
	}
	return line
}
