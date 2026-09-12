package semver

import "testing"

func TestParseVersion_Strict(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"1.4.2", false},
		{"0.0.1", false},
		{"1.5.0-rc.1", false},
		{"1.5.0-rc.1+build.5", false},
		{"v1.4.2", true},  // leading v rejected, not stripped
		{"1.4", true},     // partial version
		{"1", true},       // partial version
		{"1.4.2.1", true}, // too many components
		{"", true},
		{"1.04.2", true}, // leading zero
	}
	for _, c := range cases {
		_, err := ParseVersion(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseVersion(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
		if err != nil {
			if code, ok := err.(interface{ Code() string }); ok {
				if code.Code() != "PKG-025" {
					t.Errorf("ParseVersion(%q): expected PKG-025, got %s", c.in, code.Code())
				}
			}
		}
	}
}

func TestVersion_Compare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.0.0", 1},
		{"1.0.0", "1.0.0", 0},
		{"1.0.0-alpha", "1.0.0", -1}, // prerelease has lower precedence
		{"1.0.0", "1.0.0-alpha", 1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1}, // numeric < alphanumeric
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0+build1", "1.0.0+build2", 0}, // build metadata ignored
	}
	for _, c := range cases {
		va, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("parse %q: %v", c.a, err)
		}
		vb, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("parse %q: %v", c.b, err)
		}
		if got := va.Compare(vb); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseConstraint_Unsupported(t *testing.T) {
	cases := []string{
		"1.2.3 || 2.0.0",
		"*",
		"1.2.x",
		"^1",
		"~1.2",
		"1.2.3 - 1.5.0",
		"!=1.2.3",
		"",
		"^1.2.3\t<2.0.0",
	}
	for _, c := range cases {
		_, err := ParseConstraint(c)
		if err == nil {
			t.Errorf("ParseConstraint(%q): expected error, got nil", c)
			continue
		}
		coder, ok := err.(interface{ Code() string })
		if !ok || coder.Code() != "PKG-003" {
			t.Errorf("ParseConstraint(%q): expected PKG-003, got %v", c, err)
		}
	}
}

func TestConstraint_Satisfies_Caret(t *testing.T) {
	cases := []struct {
		constraint string
		version    string
		want       bool
	}{
		{"^1.4.2", "1.4.2", true},
		{"^1.4.2", "1.9.9", true},
		{"^1.4.2", "2.0.0", false},
		{"^1.4.2", "1.4.1", false},
		{"^0.4.2", "0.4.9", true},
		{"^0.4.2", "0.5.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},
		{"~1.4.2", "1.4.9", true},
		{"~1.4.2", "1.5.0", false},
		{"~1.4.2", "1.4.1", false},
		{">=1.4.2 <2.0.0", "1.9.9", true},
		{">=1.4.2 <2.0.0", "2.0.0", false},
		{"=1.4.2", "1.4.2", true},
		{"1.4.2", "1.4.2", true}, // bare exact
	}
	for _, c := range cases {
		constraint, err := ParseConstraint(c.constraint)
		if err != nil {
			t.Fatalf("ParseConstraint(%q): %v", c.constraint, err)
		}
		v, err := ParseVersion(c.version)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.version, err)
		}
		if got := constraint.Satisfies(v); got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", c.constraint, c.version, got, c.want)
		}
	}
}

func TestConstraint_Satisfies_PrereleaseRule(t *testing.T) {
	// 1.5.0-rc.1 does NOT satisfy ^1.4.0
	c1, err := ParseConstraint("^1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := ParseVersion("1.5.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Satisfies(v1) {
		t.Errorf("1.5.0-rc.1 should NOT satisfy ^1.4.0")
	}

	// but does satisfy >=1.5.0-rc.1 <1.6.0
	c2, err := ParseConstraint(">=1.5.0-rc.1 <1.6.0")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Satisfies(v1) {
		t.Errorf("1.5.0-rc.1 should satisfy >=1.5.0-rc.1 <1.6.0")
	}
}

func TestConstraint_Intersect_EmptyDetection(t *testing.T) {
	c1, err := ParseConstraint("^1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ParseConstraint(">=2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	merged := c1.Intersect(c2)
	// No version can satisfy both ^1.4.0 (<2.0.0) and >=2.0.0: empty intersection.
	v, _ := ParseVersion("1.9.9")
	if merged.Satisfies(v) {
		t.Errorf("expected empty intersection, but %q satisfied it", v)
	}
	v2, _ := ParseVersion("2.5.0")
	if merged.Satisfies(v2) {
		t.Errorf("expected empty intersection, but %q satisfied it", v2)
	}
}

func TestConstraint_Intersect_NonEmpty(t *testing.T) {
	c1, err := ParseConstraint("^1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ParseConstraint(">=1.5.0")
	if err != nil {
		t.Fatal(err)
	}
	merged := c1.Intersect(c2)
	v, _ := ParseVersion("1.6.0")
	if !merged.Satisfies(v) {
		t.Errorf("expected %q to satisfy intersection of %q and %q", v, c1, c2)
	}
}
