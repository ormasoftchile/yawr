package expand

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeUnset, false},
		{"eager", ModeEager, false},
		{"lazy", ModeLazy, false},
		{"auto", ModeAuto, false},
		{"EAGER", ModeUnset, true},
		{"never", ModeUnset, true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("Parse(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestPolicyResolvePrecedence(t *testing.T) {
	cases := []struct {
		name          string
		policyDefault Mode
		runbookMode   Mode
		siteMode      Mode
		want          Mode
	}{
		{"all unset -> lazy", ModeUnset, ModeUnset, ModeUnset, ModeLazy},
		{"policy default eager", ModeEager, ModeUnset, ModeUnset, ModeEager},
		{"runbook overrides policy", ModeEager, ModeLazy, ModeUnset, ModeLazy},
		{"site overrides runbook", ModeEager, ModeLazy, ModeEager, ModeEager},
		{"site overrides everything (lazy)", ModeEager, ModeEager, ModeLazy, ModeLazy},
		{"auto passes through", ModeUnset, ModeAuto, ModeUnset, ModeAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Policy{Default: c.policyDefault}
			got := p.Resolve(c.siteMode, c.runbookMode)
			if got != c.want {
				t.Errorf("Resolve(site=%q, rb=%q, default=%q) = %q want %q",
					c.siteMode, c.runbookMode, c.policyDefault, got, c.want)
			}
		})
	}
}

func TestPolicyMaterialize(t *testing.T) {
	p := Policy{AutoIncludeThreshold: 3}
	cases := []struct {
		mode  Mode
		count int
		want  Mode
	}{
		{ModeEager, 100, ModeEager},
		{ModeLazy, 0, ModeLazy},
		{ModeAuto, 0, ModeEager},
		{ModeAuto, 3, ModeEager},
		{ModeAuto, 4, ModeLazy},
	}
	for _, c := range cases {
		got := p.Materialize(c.mode, c.count)
		if got != c.want {
			t.Errorf("Materialize(%q, %d) = %q want %q", c.mode, c.count, got, c.want)
		}
	}
}

func TestPolicyMaterializeDefaultThreshold(t *testing.T) {
	p := Policy{}
	if got := p.Materialize(ModeAuto, DefaultAutoIncludeThreshold); got != ModeEager {
		t.Errorf("at threshold = eager, got %q", got)
	}
	if got := p.Materialize(ModeAuto, DefaultAutoIncludeThreshold+1); got != ModeLazy {
		t.Errorf("above threshold = lazy, got %q", got)
	}
}
