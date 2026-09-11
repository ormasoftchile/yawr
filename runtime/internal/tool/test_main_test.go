package tool

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
)

var toolsDir string

func TestMain(m *testing.M) {
	if os.Getenv("YAWR_NATIVE_HELPER") == "1" {
		os.Exit(m.Run())
	}
	root := repoRoot()
	toolsDir = filepath.Join(root, ".testtools", "internal-tool")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		panic(err)
	}

	mustBuild(root, filepath.Join(toolsDir, "echo"), "./cmd/tools/echo")
	mustBuild(root, filepath.Join(toolsDir, "fail"), "./cmd/tools/fail")
	mustBuild(root, filepath.Join(toolsDir, "slow"), "./cmd/tools/slow")
	mustBuild(root, filepath.Join(toolsDir, "json-emitter"), "./cmd/tools/json-emitter")
	mustBuild(root, filepath.Join(toolsDir, "jsonrpc-server"), "./cmd/tools/jsonrpc-server")
	mustBuild(root, filepath.Join(toolsDir, "mcp-server"), "./cmd/tools/mcp-server")
	mustBuild(root, filepath.Join(toolsDir, "yawr-stub"), "./cmd/tools/stub")

	_ = os.Setenv("YAWR_TOOLS_DIR", toolsDir)
	_ = os.Setenv("PATH", toolsDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code := m.Run()
	_ = os.RemoveAll(toolsDir)
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

func toolPath(name string) string {
	return filepath.Join(toolsDir, testutil.BinaryName(name))
}
