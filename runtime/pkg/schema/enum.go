package schema

// Package-level support for the enum string-constraint MVP
// (design/yawr/sections/03-schema-vnext.tex §Input/§Output Declarations,
// design/yawr/sections/06-tool-runtime.tex §Tool Definition Schema,
// AR-ENUM-1..15 in .squad/decisions/inbox/barbara-enum-constraint-mvp-
// architecture-ruling.md). `enum` is valid at exactly four declaration
// sites: tool action args.<name> (S1), tool action outputs.<name> (S2),
// runbook inputs.<name> (S3), runbook outputs.<name> (S4). This file
// implements the shared EnumConstraint YAML type (structural
// well-formedness, ENUM-002) and the semantic member-validation helpers
// (ENUM-003..005, ENUM-W001) used by all four sites, plus the
// NFC-normalised, order-insensitive comparison primitives used at every
// binding site (ENUM-006..009) and by the PKG-013 set-equality extension
// (AR-ENUM-8).

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
)

// EnumConstraint is the declared-order list of permitted string values for
// an enum-constrained declaration. Its custom UnmarshalYAML performs the
// AR-ENUM-3 structural well-formedness checks (ENUM-002) at the raw-node
// level, since these checks depend on distinguishing an implicit,
// unquoted, string-resolving scalar from a coerced or explicitly-tagged
// non-string scalar -- information a plain []string decode discards.
type EnumConstraint []string

// UnmarshalYAML implements AR-ENUM-3 rules 1-4 (sequence, minItems: 1, every
// item a plain string-resolving scalar, no nested collections or explicit
// non-str tags on items). Rules 5-7 (per-member whitespace/emptiness,
// duplicate/NFC, control/bidi characters) are semantic and are checked
// separately by ValidateMembers, since they require comparing across
// members rather than judging one YAML node in isolation.
func (e *EnumConstraint) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return errkit.New("ENUM-002", "enum must be a YAML sequence of strings")
	}
	if len(node.Content) == 0 {
		return errkit.New("ENUM-002", "enum must contain at least one member (minItems: 1)")
	}
	out := make([]string, 0, len(node.Content))
	for i, item := range node.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return errkit.New("ENUM-002", fmt.Sprintf(
				"enum[%d]: member must be a plain YAML string scalar (got %s); quote the value to force a string, no coercion is performed", i, describeNodeKind(item)))
		}
		out = append(out, item.Value)
	}
	*e = EnumConstraint(out)
	return nil
}

// MarshalYAML round-trips EnumConstraint as a plain string sequence.
func (e EnumConstraint) MarshalYAML() (any, error) {
	return []string(e), nil
}

func describeNodeKind(item *yaml.Node) string {
	if item.Tag != "" && item.Tag != "!!str" {
		return fmt.Sprintf("tag %s", item.Tag)
	}
	switch item.Kind {
	case yaml.SequenceNode:
		return "a nested sequence"
	case yaml.MappingNode:
		return "a nested mapping"
	default:
		return fmt.Sprintf("kind %d", item.Kind)
	}
}

// bidiFormatControls are the Unicode bidi/format control characters
// forbidden in an enum member (AR-ENUM-3 rule 7): U+200E, U+200F,
// U+202A-U+202E, U+2066-U+2069.
func isForbiddenControl(r rune) bool {
	switch {
	case r >= 0x0000 && r <= 0x001F:
		return true
	case r == 0x007F:
		return true
	case r == 0x200E || r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// ValidateMembers checks AR-ENUM-3 rules 5-7 across a declared-order member
// list: emptiness/whitespace (ENUM-003), pairwise NFC-distinctness
// (ENUM-004), UTF-8/NFC-already/control-character validity (ENUM-005). It
// returns the first well-formedness error found (fatal errors are reported
// singly per the existing PlanValidationError / ValidationError shape used
// throughout the codebase) and, separately, any ENUM-W001 case-only-distinct
// warnings (non-fatal, never mixed into the fatal return).
func ValidateMembers(members []string) (fatal error, warnings []error) {
	seenNFC := make(map[string]string, len(members)) // NFC form -> first original member
	for _, m := range members {
		if !utf8.ValidString(m) {
			return errkit.New("ENUM-005", fmt.Sprintf("enum member %q is not valid UTF-8", m)), warnings
		}
		if strings.TrimSpace(m) == "" {
			return errkit.New("ENUM-003", fmt.Sprintf("enum member %q is empty or whitespace-only", m)), warnings
		}
		if trimmed := strings.TrimSpace(m); trimmed != m {
			return errkit.New("ENUM-003", fmt.Sprintf("enum member %q has leading/trailing whitespace", m)), warnings
		}
		for _, r := range m {
			if isForbiddenControl(r) {
				return errkit.New("ENUM-005", fmt.Sprintf("enum member %q contains a forbidden control/bidi-format character U+%04X", m, r)), warnings
			}
		}
		nfc := norm.NFC.String(m)
		if nfc != m {
			return errkit.New("ENUM-005", fmt.Sprintf("enum member %q is not already in NFC normal form", m)), warnings
		}
		if first, dup := seenNFC[nfc]; dup {
			if first == m {
				return errkit.New("ENUM-004", fmt.Sprintf("duplicate enum member %q", m)), warnings
			}
			return errkit.New("ENUM-004", fmt.Sprintf("enum members %q and %q are duplicates after NFC normalisation", first, m)), warnings
		}
		seenNFC[nfc] = m
	}

	// ENUM-W001: case-only-distinct member pairs (legal, but worth a warning).
	seenLower := make(map[string]string, len(members))
	for _, m := range members {
		lower := strings.ToLower(m)
		if first, dup := seenLower[lower]; dup && first != m {
			warnings = append(warnings, errkit.New("ENUM-W001", fmt.Sprintf("enum members %q and %q are case-only-distinct", first, m)))
		} else if !dup {
			seenLower[lower] = m
		}
	}
	return nil, warnings
}

// ValidateTypeSite enforces AR-ENUM-2/C1 (ENUM-001): enum is valid only on a
// declaration resolving to type: string, and is unconditionally forbidden on
// type: secret regardless of redact/sensitive_inputs status. resolvedType is
// the declaration's own type (already defaulted to "string" by the caller
// where the schema makes string the implicit default, e.g. runbook inputs).
func ValidateTypeSite(resolvedType string) error {
	if resolvedType != "string" {
		return errkit.New("ENUM-001", fmt.Sprintf("enum is only valid on a type: string declaration (got type: %s)", resolvedType))
	}
	return nil
}

// NormalizeCandidate NFC-normalises a candidate value for enum comparison
// (AR-ENUM-4: asymmetric normalisation -- declared members must already be
// NFC; candidates from outside the file are normalised before comparison).
func NormalizeCandidate(s string) string {
	return norm.NFC.String(s)
}

// Contains reports whether candidate is a member of the enum (declared
// members, already required to be NFC) under codepoint-wise equality after
// NFC-normalising candidate (AR-ENUM-4). No case folding, no trimming.
func (e EnumConstraint) Contains(candidate string) bool {
	normalized := NormalizeCandidate(candidate)
	for _, m := range e {
		if m == normalized {
			return true
		}
	}
	return false
}

// SetEqual reports whether a and b declare the same enum SET (AR-ENUM-5:
// contract identity for PKG-013 is set equality, order-insensitive; member
// order is presentation-only and reordering is never a contract break).
func SetEqual(a, b EnumConstraint) bool {
	if len(a) != len(b) {
		return false
	}
	as := make(map[string]struct{}, len(a))
	for _, m := range a {
		as[m] = struct{}{}
	}
	for _, m := range b {
		if _, ok := as[m]; !ok {
			return false
		}
	}
	return true
}

// EnumBindingError is the typed ENUM-008 error CheckCallerInputBindings
// returns. It carries the code (via the wrapped errkit sentinel, so
// errors.Is/As against errkit.ErrENUM008 and errkit.Coder both work) and
// the declaration's location (the input name) for safe client-facing
// rendering, but deliberately never carries the caller-supplied value: a
// wire-facing ENUM-008 error message MUST NOT leak the rejected value
// (C1; AR-CE-4 §5, AR-CE-6 §4 — "no 'permitted values' or 'rejected
// value' payload field anywhere", tv-enum.yaml TV-ENUM-REDACT-* notes).
type EnumBindingError struct {
	cause *errkit.Error
	Input string
}

// Error renders a log/stderr-facing message: the input name (never a
// secret) plus the coded, value-free cause message.
func (e *EnumBindingError) Error() string {
	return fmt.Sprintf("input %q: %s", e.Input, e.cause.Error())
}

// Unwrap exposes the wrapped errkit sentinel-compatible cause.
func (e *EnumBindingError) Unwrap() error { return e.cause }

// Code implements errkit.Coder.
func (e *EnumBindingError) Code() string { return e.cause.Code() }

// Class implements errkit.Classer.
func (e *EnumBindingError) Class() string { return e.cause.Class() }

// Location returns a safe-to-render location string for this error (the
// declaration's dotted path, e.g. "inputs.env_name"). Never the value.
func (e *EnumBindingError) Location() string { return "inputs." + e.Input }

// CheckCallerInputBindings enforces ENUM-008 (AR-ENUM-7) for every
// caller-supplied value bound to a declared, enum-constrained runbook
// input, at every real caller-binding path this runtime has: `yawr run`'s
// --var flags (cmd/yawr/run.go), pkg/run.Start's Config.Variables, and
// internal/serve's RPC-supplied params.Inputs. This is the single shared
// helper hoisted from pkg/run/run.go's original ENUM-008 loop so the check
// runs identically everywhere a caller binds a value to a declared input,
// not only on the one library entry point the CLI never called
// (barbara-enum-mvp-implementation-gate.md R1).
//
// inputs is the runbook's declared Inputs map; values is caller-supplied
// name->stringified-value pairs. Only inputs present in both maps are
// checked; a value for an undeclared name is not this function's concern
// (that is a different validation, if any exists, elsewhere). Returns the
// first violation found, in a stable (sorted-by-name) order so behavior is
// deterministic across map iteration.
func CheckCallerInputBindings(inputs map[string]*Input, values map[string]string) error {
	if len(inputs) == 0 || len(values) == 0 {
		return nil
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		in, ok := inputs[name]
		if !ok || in == nil || len(in.Enum) == 0 {
			continue
		}
		val := values[name]
		if !in.Enum.Contains(val) {
			// Value is deliberately excluded from the error message (see
			// EnumBindingError doc): only the input name and the coded,
			// generic cause are carried.
			return &EnumBindingError{
				cause: errkit.New("ENUM-008", "value is not a declared enum member"),
				Input: name,
			}
		}
	}
	return nil
}

// AdvisoryEnumContains is the engine-side helper for client-side advisory
// (non-blocking) enum membership pre-checks (AR-CE-4 §2, AR-CE-5 §1-2):
// it reports whether candidate would be accepted by CheckCallerInputBindings/
// CheckArgEnums for this member list, using the exact ruled comparison
// (NFC-normalise a COPY of candidate for comparison only, codepoint-wise,
// case-sensitive — no trim, no case fold, no rewrite of candidate itself).
// It never mutates or returns a normalised form of candidate: callers that
// pre-validate for UX MUST still submit the operator's original,
// byte-for-byte input, and MUST treat this result as a hint only — final
// enforcement remains exclusively in CheckCallerInputBindings/CheckArgEnums
// at the real binding site. A client MUST NOT invent its own comparison
// algorithm (a near-miss client algorithm is worse than none, AR-CE-4 §2);
// this is the one, shared, engine-owned implementation of that algorithm.
func AdvisoryEnumContains(members EnumConstraint, candidate string) bool {
	return members.Contains(candidate)
}

// ValidateDeclaration runs the full plan/parse-time well-formedness gate for
// one enum-constrained declaration: type-site restriction (ENUM-001, C1),
// then member well-formedness (ENUM-002 is already enforced during
// UnmarshalYAML and thus not re-checked here), then ENUM-003/004/005 and
// ENUM-W001. secretType additionally forces ENUM-001 even when
// resolvedType happens to be "string" is never possible (secret is its own
// type), so ValidateTypeSite alone is sufficient -- callers pass the
// declaration's own type string directly (including "secret").
func ValidateDeclaration(resolvedType string, enum EnumConstraint) (fatal error, warnings []error) {
	if len(enum) == 0 {
		return nil, nil
	}
	if err := ValidateTypeSite(resolvedType); err != nil {
		return err, nil
	}
	return ValidateMembers([]string(enum))
}

// IsRedactedDeclName reports whether declName (a dotted declaration path,
// e.g. "inputs.env_name") matches any of gov's governance.redact patterns
// (C1: a best-effort proxy for "sensitive declaration", the same signal
// internal/planner's plan-time EnumMeta.Redacted computation uses). This is
// the single shared implementation so a declaration-carriage DTO built
// directly from a parsed (not yet planned) Runbook -- e.g.
// pkg/preview/graphdoc's input-declaration list -- redacts identically to
// the plan-time ValidatedPlan.EnumConstraints metadata, without either
// side re-implementing (and risking drifting from) the other's redaction
// pattern matching. gov may be nil (no governance configured).
func IsRedactedDeclName(declName string, gov *GovernanceConfig) bool {
	if gov == nil || len(gov.Redact) == 0 {
		return false
	}
	for _, r := range gov.Redact {
		if r.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		if re.MatchString(declName) {
			return true
		}
	}
	return false
}
