package duration

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Duration
		wantErr bool
	}{
		{"15m", Duration{15, "m"}, false},
		{"2h", Duration{2, "h"}, false},
		{"7d", Duration{7, "d"}, false},
		{"3w", Duration{3, "w"}, false},
		{"6M", Duration{6, "M"}, false},
		{"1y", Duration{1, "y"}, false},
		{"", Duration{}, true},
		{"7", Duration{}, true},
		{"d", Duration{}, true},
		{"7s", Duration{}, true},  // unknown unit
		{"-1d", Duration{}, true}, // sign rejected
		{"7 d", Duration{}, true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("Parse(%q) err = %v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestToSeconds(t *testing.T) {
	cases := []struct {
		in   Duration
		want int64
	}{
		{Duration{15, "m"}, 900},
		{Duration{2, "h"}, 7200},
		{Duration{7, "d"}, 7 * 86_400},
		{Duration{1, "w"}, 7 * 86_400},
		{Duration{1, "M"}, 30 * 86_400},
		{Duration{1, "y"}, 365 * 86_400},
		{Duration{}, 0},
	}
	for _, c := range cases {
		if got := c.in.ToSeconds(); got != c.want {
			t.Errorf("(%+v).ToSeconds() = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestToDays(t *testing.T) {
	cases := []struct {
		in   Duration
		want int
	}{
		{Duration{15, "m"}, 0},
		{Duration{23, "h"}, 0},
		{Duration{24, "h"}, 1},
		{Duration{7, "d"}, 7},
		{Duration{2, "w"}, 14},
	}
	for _, c := range cases {
		if got := c.in.ToDays(); got != c.want {
			t.Errorf("(%+v).ToDays() = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, s := range []string{"15m", "2h", "7d", "3w", "6M", "1y"} {
		d, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if d.String() != s {
			t.Errorf("round-trip mismatch: %q -> %q", s, d.String())
		}
	}
}
