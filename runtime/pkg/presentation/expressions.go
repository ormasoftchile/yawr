package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
)

const ExpressionSchemaVersion = "yawr.expression-resolve/v1"
const ExpressionResolverVersion = "yawr.core-expression/v1"
const ExpressionGrammarVersion = "yawr-expression/v2"
const MaxExpressionValueUnits = 32768
const MaxExpressionTokens = 65536
const MaxExpressionDepth = 128

type ExpressionMode string
type ExpressionClass string

const (
	ExpressionGXL   ExpressionMode = "gxl"
	ExpressionGIS   ExpressionMode = "gis"
	ExpressionRegex ExpressionMode = "regex"
)

type ExpressionToken struct {
	Start int             `json:"start"`
	End   int             `json:"end"`
	Class ExpressionClass `json:"class"`
}
type ExpressionValue struct {
	Mode       ExpressionMode    `json:"mode"`
	TextLength int               `json:"text_length"`
	TextDigest string            `json:"text_digest"`
	Tokens     []ExpressionToken `json:"tokens"`
}
type ExpressionRegion struct {
	ExpressionValue
	YAMLPath string `json:"yaml_path"`
	Range    Range  `json:"range"`
}
type ExpressionResolveRequest = Request
type ExpressionResolveReply struct {
	SchemaVersion   string             `json:"schema_version"`
	ResolverVersion string             `json:"resolver_version"`
	GrammarVersion  string             `json:"grammar_version"`
	RequestID       string             `json:"request_id"`
	Context         Context            `json:"context"`
	Document        Document           `json:"document"`
	Status          string             `json:"status"`
	Reason          string             `json:"reason,omitempty"`
	Regions         []ExpressionRegion `json:"regions"`
}
type ExpressionDetailValue struct {
	ExpressionValue
	Path string `json:"path"`
}
type ExpressionPresentation struct {
	Version        int                     `json:"version"`
	GrammarVersion string                  `json:"grammar_version"`
	Values         []ExpressionDetailValue `json:"values"`
}
type ExpressionCapabilities struct {
	SchemaVersion     string           `json:"schema_version"`
	ResolverVersion   string           `json:"resolver_version"`
	GrammarVersion    string           `json:"grammar_version"`
	Modes             []ExpressionMode `json:"modes"`
	MaxValueCodeUnits int              `json:"max_value_code_units"`
	MaxRegions        int              `json:"max_regions"`
	MaxTokens         int              `json:"max_tokens"`
}

func ExpressionsCapabilities() ExpressionCapabilities {
	return ExpressionCapabilities{"yawr.expression-capabilities/v1", ExpressionResolverVersion, ExpressionGrammarVersion, []ExpressionMode{ExpressionGXL, ExpressionGIS, ExpressionRegex}, MaxExpressionValueUnits, MaxEntries, MaxExpressionTokens}
}
func DecodeExpressionRequest(r io.Reader) (ExpressionResolveRequest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(data) > MaxBytes {
		return Request{}, errors.New("invalid-request")
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if json.Unmarshal(data, &header) != nil {
		return Request{}, errors.New("invalid-request")
	}
	if header.SchemaVersion != ExpressionSchemaVersion {
		return Request{}, errors.New("unsupported-version")
	}
	return decodeRequestVersion(bytes.NewReader(data), header.SchemaVersion)
}

func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// HighlightExpression produces display metadata without compiling or evaluating.
// Boolean compatibility wrappers are enabled only by an established boolean site.
func HighlightExpression(ctx context.Context, text string, mode ExpressionMode, booleanSite bool) (ExpressionValue, bool) {
	return highlightExpression(ctx, text, mode, booleanSite, parser.Highlight)
}

func highlightExpression(ctx context.Context, text string, mode ExpressionMode, booleanSite bool, highlight func(context.Context, string) []parser.HighlightToken) (ExpressionValue, bool) {
	if ctx.Err() != nil || len(text) > MaxExpressionValueUnits*3 {
		return ExpressionValue{Mode: mode, Tokens: []ExpressionToken{}}, false
	}
	out := ExpressionValue{Mode: mode, TextLength: utf16Length(text), TextDigest: Digest([]byte(text)), Tokens: []ExpressionToken{}}
	if ctx.Err() != nil || !utf8.ValidString(text) || out.TextLength > MaxExpressionValueUnits || (mode != ExpressionGXL && mode != ExpressionGIS && mode != ExpressionRegex) {
		return out, false
	}
	// A byte-to-code-unit table also guarantees astral characters are indivisible.
	units := make([]int, len(text)+1)
	n := 0
	for i, r := range text {
		units[i] = n
		n++
		if r > 0xffff {
			n++
		}
	}
	units[len(text)] = n
	add := func(start, end int, class string) {
		if start < end {
			out.Tokens = append(out.Tokens, ExpressionToken{units[start], units[end], ExpressionClass(class)})
		}
	}
	lex := func(start, end int) {
		for _, t := range highlight(ctx, text[start:end]) {
			add(start+t.Start, start+t.End, t.Class)
		}
	}
	if mode == ExpressionGIS || mode == ExpressionRegex {
		blocks := gis.ScanExpressions(ctx, text)
		if mode == ExpressionRegex {
			var literals []gis.LiteralBlock
			literals, blocks = gis.ScanPresentation(ctx, text)
			for _, literal := range literals {
				for _, t := range parser.HighlightRegex(ctx, literal.Text) {
					add(literal.Offsets[t.Start], literal.Offsets[t.End], t.Class)
				}
			}
		}
		for _, b := range blocks {
			add(b.Start, b.ExpressionStart, "interpolation")
			lex(b.ExpressionStart, b.ExpressionEnd)
			if b.Closed {
				add(b.ExpressionEnd, b.ExpressionEnd+1, "interpolation")
			}
			sort.Slice(out.Tokens, func(i, j int) bool { return out.Tokens[i].Start < out.Tokens[j].Start })
		}
	} else {
		trimmed := strings.TrimSpace(text)
		if booleanSite && strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") {
			start := strings.Index(text, trimmed)
			end := start + len(trimmed)
			add(start, start+2, "interpolation")
			lex(start+2, end-2)
			add(end-2, end, "interpolation")
		} else {
			lex(0, len(text))
		}
	}
	return out, ctx.Err() == nil && len(out.Tokens) <= MaxExpressionTokens
}

func ResolveExpressions(ctx context.Context, req ExpressionResolveRequest) ExpressionResolveReply {
	reply := ExpressionResolveReply{SchemaVersion: ExpressionSchemaVersion, ResolverVersion: ExpressionResolverVersion, GrammarVersion: ExpressionGrammarVersion, RequestID: req.RequestID, Context: req.Context, Document: Document{URI: req.Document.URI, Version: req.Document.Version}, Status: "resolved", Regions: []ExpressionRegion{}}
	fail := func(reason string) ExpressionResolveReply {
		reply.Status, reply.Reason = "unavailable", reason
		reply.Regions = []ExpressionRegion{}
		return reply
	}
	data, err := json.Marshal(req)
	if err != nil || len(data) > MaxBytes {
		return fail("invalid-request")
	}
	if _, err := DecodeExpressionRequest(strings.NewReader(string(data))); err != nil {
		return fail("invalid-request")
	}
	if ctx.Err() != nil {
		return fail("limit-exceeded")
	}
	root, complete := parseSource(req.Document.Text)
	if root == nil || !uniqueMappingKeys(root) {
		return fail("incomplete-source")
	}
	visitor := expressionSourceVisitor{ctx: ctx, text: req.Document.Text, regions: []ExpressionRegion{}, typed: true}
	if !visitor.check(root, 0) {
		if visitor.limit {
			return fail("limit-exceeded")
		}
		return fail("incomplete-source")
	}
	visitor.runbook(root)
	if visitor.limit || ctx.Err() != nil {
		return fail("limit-exceeded")
	}
	if visitor.invalid {
		return fail("incomplete-source")
	}
	if !complete && len(visitor.regions) == 0 {
		return fail("incomplete-source")
	}
	reply.Regions = visitor.regions
	sortExpressionRegions(reply.Regions)
	for i := 1; i < len(reply.Regions); i++ {
		if reply.Regions[i-1].Range.End > reply.Regions[i].Range.Start {
			return fail("incomplete-source")
		}
	}
	data, err = json.Marshal(reply)
	if err != nil || len(data) > MaxBytes {
		return fail("limit-exceeded")
	}
	return reply
}
