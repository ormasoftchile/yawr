package pjvm

import (
	"errors"
	"math"
	"testing"
)

func TestFromJSONRoundTripCanonical(t *testing.T) {
	v, err := FromJSON([]byte(`{"z":1.0,"a":[true,null,"x"],"n":-0}`))
	if err != nil {
		t.Fatalf("FromJSON() error = %v", err)
	}
	got, err := v.ToCanonicalJSON()
	if err != nil {
		t.Fatalf("ToCanonicalJSON() error = %v", err)
	}
	if string(got) != `{"a":[true,null,"x"],"n":0,"z":1}` {
		t.Fatalf("canonical JSON = %s", got)
	}
}

func TestFromYAMLNullStringDistinction(t *testing.T) {
	nullValue, err := FromYAML([]byte("null\n"))
	if err != nil {
		t.Fatalf("FromYAML(null) error = %v", err)
	}
	stringValue, err := FromYAML([]byte("\"null\"\n"))
	if err != nil {
		t.Fatalf("FromYAML(quoted null) error = %v", err)
	}
	if nullValue.Kind() != KindNull {
		t.Fatalf("unquoted null kind = %s", nullValue.Kind())
	}
	if got, ok := stringValue.StringValue(); !ok || got != "null" {
		t.Fatalf("quoted null = (%q, %v)", got, ok)
	}
	if nullValue.Equal(stringValue) {
		t.Fatal("YAML null and string \"null\" must differ")
	}
}

func TestFromAnyNestedRoundTrip(t *testing.T) {
	v, err := FromAny(map[string]any{"b": []any{1, "two"}, "a": map[string]any{"x": false}})
	if err != nil {
		t.Fatalf("FromAny() error = %v", err)
	}
	got, _ := v.ToCanonicalJSON()
	if string(got) != `{"a":{"x":false},"b":[1,"two"]}` {
		t.Fatalf("canonical JSON = %s", got)
	}
}

func TestEqualityNumberSemantics(t *testing.T) {
	one, err := FromAny(1)
	if err != nil {
		t.Fatal(err)
	}
	onePointZero, err := FromAny(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if !one.Equal(onePointZero) {
		t.Fatal("1 and 1.0 must be equal")
	}
}

func TestTruthiness(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want bool
	}{
		{"null", Null(), false},
		{"false", Bool(false), false},
		{"true", Bool(true), true},
		{"zero", mustValue(Number(0)), false},
		{"nonzero", mustValue(Number(2)), true},
		{"empty string", mustValue(NewString("")), false},
		{"string", mustValue(NewString("x")), true},
		{"empty array", Array(nil), true},
		{"empty object", Object(nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Truthy(); got != tc.want {
				t.Fatalf("Truthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNumberRejectsNaNAndInfinity(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Number(f); !errors.Is(err, ErrNonFiniteNumber) {
			t.Fatalf("Number(%v) error = %v, want ErrNonFiniteNumber", f, err)
		}
	}
}

func TestSafeIntegerRange(t *testing.T) {
	if _, err := FromAny(int64(maxSafeInteger)); err != nil {
		t.Fatalf("max safe integer rejected: %v", err)
	}
	if _, err := FromAny(int64(maxSafeInteger + 1)); err == nil {
		t.Fatal("integer above 2^53 safe range accepted")
	}
	if _, err := FromJSON([]byte(`9007199254740993`)); err == nil {
		t.Fatal("JSON integer above 2^53 safe range accepted")
	}
}

func TestNumberFormatting(t *testing.T) {
	cases := map[float64]string{
		42.0: "42",
		3.14: "3.14",
		1e20: "100000000000000000000",
		1e21: "1e+21",
		-0.0: "0",
	}
	for input, want := range cases {
		v, err := Number(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != want {
			t.Fatalf("Number(%v).String() = %q, want %q", input, got, want)
		}
	}
}

func TestStringDisplay(t *testing.T) {
	v, err := FromJSON([]byte(`{"z":1,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != `{"a":"x","z":1}` {
		t.Fatalf("String() = %q", got)
	}
	s, _ := NewString("plain")
	if got := s.String(); got != "plain" {
		t.Fatalf("string display = %q", got)
	}
}

func mustValue(v Value, err error) Value {
	if err != nil {
		panic(err)
	}
	return v
}
