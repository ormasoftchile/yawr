package platform_test

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func TestRealPlatform_ExecSuffix(t *testing.T) {
	p := platform.Real()
	suffix := p.ExecSuffix()
	if runtime.GOOS == "windows" {
		if suffix != ".exe" {
			t.Errorf("expected .exe on Windows, got %q", suffix)
		}
	} else {
		if suffix != "" {
			t.Errorf("expected empty suffix on %s, got %q", runtime.GOOS, suffix)
		}
	}
}

func TestRealPlatform_AllowedSignals(t *testing.T) {
	p := platform.Real()
	sigs := p.AllowedSignals()
	for _, s := range sigs {
		if s == "SIGINT" {
			return
		}
	}
	t.Errorf("expected AllowedSignals to contain SIGINT, got %v", sigs)
}

func TestFakePlatform_AllowedSignals(t *testing.T) {
	f := platform.NewFakePlatform()
	f.Signals = []string{"SIGINT", "SIGUSR1"}
	sigs := f.AllowedSignals()
	if len(sigs) != 2 || sigs[0] != "SIGINT" || sigs[1] != "SIGUSR1" {
		t.Errorf("unexpected signals: %v", sigs)
	}
}

func TestFakePlatform_NewlineNormalizer(t *testing.T) {
	// FakePlatform is a Unix stand-in: NewlineNormalizer is a no-op.
	// Wrap with the real crlfWriter logic via a Windows-mode fake to verify CRLF stripping.
	var buf bytes.Buffer
	// Simulate Windows CRLF stripping by using realPlatform logic directly.
	// We test by writing CRLF to a crlfWriter equivalent embedded in the fake.
	// Since FakePlatform.NewlineNormalizer is a no-op, verify pass-through.
	f := platform.NewFakePlatform()
	w := f.NewlineNormalizer(&buf)
	input := "line1\r\nline2\r\n"
	w.Write([]byte(input)) //nolint:errcheck
	got := buf.String()
	// On fake (Unix default) it passes through unchanged.
	if got != input {
		t.Errorf("FakePlatform NewlineNormalizer should be a no-op, got %q", got)
	}

	// Verify CRLF normalization using Real() on Windows or by exercising crlfWriter directly.
	// On non-Windows we exercise the Real() no-op path.
	var buf2 bytes.Buffer
	rp := platform.Real()
	w2 := rp.NewlineNormalizer(&buf2)
	crlfInput := "hello\r\nworld\r\n"
	w2.Write([]byte(crlfInput)) //nolint:errcheck
	result := buf2.String()
	if runtime.GOOS == "windows" {
		if strings.Contains(result, "\r\n") {
			t.Errorf("Real NewlineNormalizer should strip CRLF on Windows, got %q", result)
		}
	} else {
		// No-op on Unix: passes through unchanged.
		if result != crlfInput {
			t.Errorf("Real NewlineNormalizer should be no-op on Unix, got %q", result)
		}
	}
}
