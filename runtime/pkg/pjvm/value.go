// Package pjvm implements the YAWR Portable JSON Value Model.
package pjvm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Kind identifies a PJVM value kind.
type Kind string

const (
	// KindNull is the PJVM null kind.
	KindNull Kind = "null"
	// KindBool is the PJVM boolean kind.
	KindBool Kind = "boolean"
	// KindNumber is the PJVM finite float64 number kind.
	KindNumber Kind = "number"
	// KindString is the PJVM UTF-8 string kind.
	KindString Kind = "string"
	// KindArray is the PJVM array kind.
	KindArray Kind = "array"
	// KindObject is the PJVM object kind.
	KindObject Kind = "object"
)

// Value is a single value in the Portable JSON Value Model.
type Value struct {
	kind Kind
	b    bool
	n    float64
	s    string
	a    []Value
	o    map[string]Value
}

// ErrNonFiniteNumber reports a NaN or Infinity number rejected by PJVM.
var ErrNonFiniteNumber = errors.New("pjvm: non-finite number")

// ErrUnsupportedType reports a Go value that cannot be converted to PJVM.
var ErrUnsupportedType = errors.New("pjvm: unsupported type")

const maxSafeInteger = 1 << 53

// Null returns the PJVM null value.
func Null() Value { return Value{kind: KindNull} }

// Bool returns a PJVM boolean value.
func Bool(v bool) Value { return Value{kind: KindBool, b: v} }

// Number returns a PJVM number, rejecting NaN and Infinity.
func Number(v float64) (Value, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Null(), fmt.Errorf("%w: %v", ErrNonFiniteNumber, v)
	}
	if v == 0 {
		v = 0
	}
	return Value{kind: KindNumber, n: v}, nil
}

// NewString returns a PJVM string value, rejecting invalid UTF-8.
func NewString(v string) (Value, error) {
	if !utf8.ValidString(v) {
		return Null(), fmt.Errorf("pjvm: invalid UTF-8 string")
	}
	return Value{kind: KindString, s: v}, nil
}

// Array returns a PJVM array value.
func Array(v []Value) Value {
	out := make([]Value, len(v))
	copy(out, v)
	return Value{kind: KindArray, a: out}
}

// Object returns a PJVM object value.
func Object(v map[string]Value) Value {
	out := make(map[string]Value, len(v))
	for k, val := range v {
		out[k] = val
	}
	return Value{kind: KindObject, o: out}
}

// FromJSON decodes JSON bytes using PJVM semantics.
func FromJSON(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return Null(), fmt.Errorf("pjvm: decode JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return Null(), fmt.Errorf("pjvm: decode JSON trailing data: %w", err)
		}
		return Null(), fmt.Errorf("pjvm: decode JSON: trailing data")
	}
	return FromAny(raw)
}

// FromYAML decodes YAML bytes using PJVM semantics.
func FromYAML(data []byte) (Value, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return Null(), fmt.Errorf("pjvm: decode YAML: %w", err)
	}
	if len(node.Content) == 1 && node.Kind == yaml.DocumentNode {
		return fromYAMLNode(node.Content[0])
	}
	return fromYAMLNode(&node)
}

// FromAny converts a JSON/YAML-like Go value to PJVM.
func FromAny(v any) (Value, error) {
	switch x := v.(type) {
	case nil:
		return Null(), nil
	case Value:
		return x, nil
	case bool:
		return Bool(x), nil
	case string:
		return NewString(x)
	case json.Number:
		return numberFromString(string(x))
	case float64:
		return Number(x)
	case float32:
		return Number(float64(x))
	case int:
		return numberFromInt(int64(x))
	case int8:
		return numberFromInt(int64(x))
	case int16:
		return numberFromInt(int64(x))
	case int32:
		return numberFromInt(int64(x))
	case int64:
		return numberFromInt(x)
	case uint:
		return numberFromUint(uint64(x))
	case uint8:
		return numberFromUint(uint64(x))
	case uint16:
		return numberFromUint(uint64(x))
	case uint32:
		return numberFromUint(uint64(x))
	case uint64:
		return numberFromUint(x)
	case []any:
		out := make([]Value, len(x))
		for i, elem := range x {
			val, err := FromAny(elem)
			if err != nil {
				return Null(), fmt.Errorf("pjvm: array[%d]: %w", i, err)
			}
			out[i] = val
		}
		return Array(out), nil
	case []Value:
		return Array(x), nil
	case map[string]any:
		out := make(map[string]Value, len(x))
		for k, elem := range x {
			val, err := FromAny(elem)
			if err != nil {
				return Null(), fmt.Errorf("pjvm: object[%q]: %w", k, err)
			}
			out[k] = val
		}
		return Object(out), nil
	case map[string]Value:
		return Object(x), nil
	case map[any]any:
		out := make(map[string]Value, len(x))
		for k, elem := range x {
			ks, ok := k.(string)
			if !ok {
				return Null(), fmt.Errorf("%w: object key %T", ErrUnsupportedType, k)
			}
			val, err := FromAny(elem)
			if err != nil {
				return Null(), fmt.Errorf("pjvm: object[%q]: %w", ks, err)
			}
			out[ks] = val
		}
		return Object(out), nil
	default:
		return Null(), fmt.Errorf("%w: %T", ErrUnsupportedType, v)
	}
}

// Kind returns the PJVM kind.
func (v Value) Kind() Kind { return v.kind }

// BoolValue returns the boolean payload and true when this is a boolean.
func (v Value) BoolValue() (bool, bool) { return v.b, v.kind == KindBool }

// NumberValue returns the number payload and true when this is a number.
func (v Value) NumberValue() (float64, bool) { return v.n, v.kind == KindNumber }

// StringValue returns the string payload and true when this is a string.
func (v Value) StringValue() (string, bool) { return v.s, v.kind == KindString }

// ArrayValue returns a copy of the array payload and true when this is an array.
func (v Value) ArrayValue() ([]Value, bool) {
	if v.kind != KindArray {
		return nil, false
	}
	out := make([]Value, len(v.a))
	copy(out, v.a)
	return out, true
}

// ObjectValue returns a copy of the object payload and true when this is an object.
func (v Value) ObjectValue() (map[string]Value, bool) {
	if v.kind != KindObject {
		return nil, false
	}
	out := make(map[string]Value, len(v.o))
	for k, val := range v.o {
		out[k] = val
	}
	return out, true
}

// Equal reports deep PJVM equality.
func (v Value) Equal(other Value) bool {
	if v.kind != other.kind {
		return false
	}
	switch v.kind {
	case KindNull:
		return true
	case KindBool:
		return v.b == other.b
	case KindNumber:
		return v.n == other.n
	case KindString:
		return v.s == other.s
	case KindArray:
		if len(v.a) != len(other.a) {
			return false
		}
		for i := range v.a {
			if !v.a[i].Equal(other.a[i]) {
				return false
			}
		}
		return true
	case KindObject:
		if len(v.o) != len(other.o) {
			return false
		}
		for k, val := range v.o {
			if !val.Equal(other.o[k]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Truthy reports the P1 truthiness helper value.
func (v Value) Truthy() bool {
	switch v.kind {
	case KindNull:
		return false
	case KindBool:
		return v.b
	case KindNumber:
		return v.n != 0
	case KindString:
		return v.s != ""
	default:
		return true
	}
}

// String returns the PJVM display string.
func (v Value) String() string {
	switch v.kind {
	case KindNull:
		return "null"
	case KindBool:
		if v.b {
			return "true"
		}
		return "false"
	case KindNumber:
		return formatNumber(v.n)
	case KindString:
		return v.s
	case KindArray, KindObject:
		data, err := v.ToCanonicalJSON()
		if err != nil {
			return "<invalid PJVM value>"
		}
		return string(data)
	default:
		return "<invalid PJVM value>"
	}
}

// ToCanonicalJSON serializes v as deterministic JSON with sorted object keys.
func (v Value) ToCanonicalJSON() ([]byte, error) {
	var b strings.Builder
	if err := writeJSON(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func fromYAMLNode(node *yaml.Node) (Value, error) {
	if node == nil {
		return Null(), nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return Null(), nil
		}
		return fromYAMLNode(node.Content[0])
	case yaml.MappingNode:
		out := make(map[string]Value, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			val, err := fromYAMLNode(node.Content[i+1])
			if err != nil {
				return Null(), fmt.Errorf("pjvm: object[%q]: %w", key, err)
			}
			out[key] = val
		}
		return Object(out), nil
	case yaml.SequenceNode:
		out := make([]Value, len(node.Content))
		for i, child := range node.Content {
			val, err := fromYAMLNode(child)
			if err != nil {
				return Null(), fmt.Errorf("pjvm: array[%d]: %w", i, err)
			}
			out[i] = val
		}
		return Array(out), nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return Null(), nil
		case "!!bool":
			return Bool(node.Value == "true" || node.Value == "True" || node.Value == "TRUE"), nil
		case "!!int", "!!float":
			return numberFromString(node.Value)
		default:
			return NewString(node.Value)
		}
	case yaml.AliasNode:
		return fromYAMLNode(node.Alias)
	default:
		return Null(), fmt.Errorf("pjvm: unsupported YAML node kind %d", node.Kind)
	}
}

func numberFromString(s string) (Value, error) {
	if !strings.ContainsAny(s, ".eE") {
		if i, ok := new(big.Int).SetString(s, 10); ok {
			limit := big.NewInt(maxSafeInteger)
			negLimit := new(big.Int).Neg(limit)
			if i.Cmp(limit) > 0 || i.Cmp(negLimit) < 0 {
				return Null(), fmt.Errorf("pjvm: integer %s outside ±2^53 safe range", s)
			}
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Null(), fmt.Errorf("pjvm: parse number %q: %w", s, err)
	}
	return Number(f)
}

func numberFromInt(i int64) (Value, error) {
	if i > maxSafeInteger || i < -maxSafeInteger {
		return Null(), fmt.Errorf("pjvm: integer %d outside 2^53 safe range", i)
	}
	return Number(float64(i))
}

func numberFromUint(u uint64) (Value, error) {
	if u > maxSafeInteger {
		return Null(), fmt.Errorf("pjvm: integer %d outside 2^53 safe range", u)
	}
	return Number(float64(u))
}

func writeJSON(b *strings.Builder, v Value) error {
	switch v.kind {
	case KindNull:
		b.WriteString("null")
	case KindBool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case KindNumber:
		b.WriteString(formatNumber(v.n))
	case KindString:
		enc, err := json.Marshal(v.s)
		if err != nil {
			return err
		}
		b.Write(enc)
	case KindArray:
		b.WriteByte('[')
		for i, elem := range v.a {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJSON(b, elem); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case KindObject:
		keys := make([]string, 0, len(v.o))
		for k := range v.o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(enc)
			b.WriteByte(':')
			if err := writeJSON(b, v.o[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("pjvm: invalid kind %q", v.kind)
	}
	return nil
}

func formatNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if f == math.Trunc(f) && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strings.ReplaceAll(strconv.FormatFloat(f, 'g', -1, 64), "E", "e")
}
