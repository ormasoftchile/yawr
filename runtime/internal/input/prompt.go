package input

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

// PromptProvider resolves values by reading from a reader.
type PromptProvider struct {
	reader io.Reader
	writer io.Writer
}

// NewPromptProvider constructs a PromptProvider for the given streams.
func NewPromptProvider(r io.Reader, w io.Writer) *PromptProvider {
	if r == nil {
		r = os.Stdin
	}
	if w == nil {
		w = os.Stderr
	}
	return &PromptProvider{reader: r, writer: w}
}

// Provide resolves the request by reading a line of input.
func (p *PromptProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.writer != nil && req.VarName != "" {
		if _, err := fmt.Fprintf(p.writer, "%s: ", req.VarName); err != nil {
			return nil, err
		}
	}
	reader := bufio.NewReader(p.reader)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line = strings.TrimSpace(line)
	if err := validateEnum(req.Schema, line); err != nil {
		return nil, err
	}
	return ensureResponse(req, &inputpkg.InputResponse{
		Value: line,
	}, p.Name()), nil
}

// Name returns the provider identifier.
func (p *PromptProvider) Name() string {
	return "prompt"
}

func validateEnum(schema map[string]any, value string) error {
	if schema == nil {
		return nil
	}
	raw, ok := schema["enum"]
	if !ok {
		return nil
	}
	switch vals := raw.(type) {
	case []any:
		for _, v := range vals {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("prompt: enum must contain only strings")
			}
			if s == value {
				return nil
			}
		}
	default:
		return fmt.Errorf("prompt: enum must be list of strings")
	}
	return fmt.Errorf("prompt: invalid value %q", value)
}
