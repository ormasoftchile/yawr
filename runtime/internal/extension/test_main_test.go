package extension

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

var extDir string

func TestMain(m *testing.M) {
	root := repoRoot()
	extDir = filepath.Join(root, ".testextensions", "hello-ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		panic(err)
	}
	mustBuild(root, filepath.Join(extDir, "hello-ext"), "./cmd/extensions/hello-ext")
	manifestSrc := filepath.Join(root, "cmd", "extensions", "hello-ext", "yawr-extension.yaml")
	manifestDst := filepath.Join(extDir, "yawr-extension.yaml")
	if data, err := os.ReadFile(manifestSrc); err == nil {
		if err := os.WriteFile(manifestDst, data, 0o644); err != nil {
			panic(err)
		}
	} else {
		panic(err)
	}
	_ = os.Setenv("YAWR_EXT_DIR", extDir)
	_ = os.Setenv("PATH", extDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	_ = os.RemoveAll(filepath.Join(root, ".testextensions"))
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func mustBuild(root, output, pkg string) {
	if _, err := testutil.BuildGoBinary(root, output, pkg); err != nil {
		panic(err)
	}
}
