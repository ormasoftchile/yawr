//go:build windows && amd64

package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/testutil"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"golang.org/x/sys/windows"
)

func TestNativeFileOnlyTransportExecutesAppContainer(t *testing.T) {
	root, _, def := fileOnlyFixtureDef(t)
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("staged-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAWR_FILE_ONLY_PARENT_SENTINEL", "must-not-inherit")
	def.Inputs = []string{"input.txt"}
	def.Actions["run"].Argv = []string{"input.txt"}
	result, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil)
	if err != nil {
		t.Fatalf("Invoke: %v; stderr=%s", err, resultStderr(result))
	}
	if result.Output["row_count"] != int64(1) {
		t.Fatalf("typed output = %#v", result.Output)
	}
}

func TestNativeFileOnlyRejectsDigestAndPaths(t *testing.T) {
	root, _, def := fileOnlyFixtureDef(t)
	def.SHA256 = strings.Repeat("0", 64)
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("digest mismatch not rejected: %v", err)
	}
	for _, command := range []string{filepath.Join(root, "fixture.exe"), `..\fixture.exe`} {
		def.SHA256 = strings.Repeat("0", 64)
		def.Command = command
		if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err == nil {
			t.Fatalf("unsafe command %q accepted", command)
		}
	}
	link := filepath.Join(root, "linked.exe")
	if err := os.Symlink(filepath.Join(root, "fixture.exe"), link); err != nil {
		t.Fatalf("create reparse test fixture: %v", err)
	}
	def.Command = "linked.exe"
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err == nil || !strings.Contains(err.Error(), "reparse") {
		t.Fatalf("reparse command not rejected: %v", err)
	}
}

func TestNativeFileOnlyDeniesNetworkHostAndChild(t *testing.T) {
	_, _, def := fileOnlyFixtureDef(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			conn.Close()
			accepted <- struct{}{}
		}
	}()
	def.Actions["run"].Argv = []string{"network", listener.Addr().String()}
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
		t.Fatal("AppContainer connected to loopback listener")
	default:
	}

	sentinel := filepath.Join(t.TempDir(), "host-sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	def.Actions["run"].Argv = []string{"host", sentinel}
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(sentinel)
	if string(data) != "unchanged" {
		t.Fatalf("host sentinel changed: %q", data)
	}

	def.Actions["run"].Argv = []string{"child", filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")}
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err != nil {
		t.Fatal(err)
	}
}

func TestNativeFileOnlyBoundsAndTerminates(t *testing.T) {
	_, _, def := fileOnlyFixtureDef(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	def.Actions["run"].Argv = []string{"sleep"}
	if _, err := (&NativeFileOnlyTransport{}).Invoke(ctx, def, "run", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	def.Actions["run"].Argv = []string{"overflow"}
	if _, err := (&NativeFileOnlyTransport{}).Invoke(context.Background(), def, "run", nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("overflow = %v", err)
	}
}

func TestNativeFileOnlyProtectedExecutableRejectsReplacement(t *testing.T) {
	root, executable, def := fileOnlyFixtureDef(t)
	sandbox := t.TempDir()
	staged, digest, err := stagePackageFile(root, def.Command, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if digest != def.SHA256 {
		t.Fatalf("staged digest = %s, want %s", digest, def.SHA256)
	}
	protected, err := protectStagedExecutable(staged, def.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(sandbox, "replacement.exe")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("replacement"), 0o600); err == nil {
		t.Fatal("verified executable remained writable while protected")
	}
	if err := os.Rename(replacement, staged); err == nil {
		t.Fatal("verified executable remained replaceable while protected")
	}
	if err := os.Remove(staged); err == nil {
		t.Fatal("verified executable remained deletable while protected")
	}
	if err := protected.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, staged); err != nil {
		t.Fatalf("replacement attempt was not valid after protection released: %v", err)
	}
	if _, err := protectStagedExecutable(staged, def.SHA256); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("replacement acquired before protection did not fail closed: %v", err)
	}
	if len(executable) == 0 {
		t.Fatal("fixture executable is empty")
	}
}

func TestNativeFileOnlyExplicitHandleAllowlist(t *testing.T) {
	if _, err := os.Stat("handle-probe.txt"); err == nil {
		probeInheritedHandle(t)
		return
	}

	sandbox := t.TempDir()
	command := filepath.Join(sandbox, "handle-probe.exe")
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenFile(command, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sandbox, "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}

	sentinelPath := filepath.Join(t.TempDir(), "inheritable-sentinel.txt")
	pathPtr, err := windows.UTF16PtrFromString(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	sa := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	sentinel, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		&sa,
		windows.CREATE_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(sentinel)
	probe := strconv.FormatUint(uint64(sentinel), 10) + "\n" + sentinelPath
	if err := os.WriteFile(filepath.Join(sandbox, "handle-probe.txt"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exitCode, err := executeFileOnlyWithEnvironment(
		context.Background(),
		command,
		[]string{"-test.run=^TestNativeFileOnlyExplicitHandleAllowlist$"},
		sandbox,
		fileOnlyOutputLimit,
		map[string]string{"YAWR_NATIVE_HELPER": "1"},
	)
	if err != nil || exitCode != 0 {
		t.Fatalf("handle probe: exit=%d err=%v stdout=%s stderr=%s", exitCode, err, stdout, stderr)
	}
}

func probeInheritedHandle(t *testing.T) {
	data, err := os.ReadFile("handle-probe.txt")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(data), "\n", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid handle probe: %q", data)
	}
	value, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var duplicate windows.Handle
	err = windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(value),
		windows.CurrentProcess(),
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	)
	if err != nil {
		return
	}
	defer windows.CloseHandle(duplicate)
	path, err := finalPath(duplicate)
	if err != nil {
		return
	}
	if strings.EqualFold(filepath.Clean(path), filepath.Clean(parts[1])) {
		t.Fatalf("inheritable sentinel handle %d crossed the process boundary", value)
	}
}

func fileOnlyFixtureDef(t *testing.T) (string, []byte, toolpkg.ToolDef) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "fixture.exe")
	if _, err := testutil.BuildGoBinary(repoRoot(), executable, "./internal/tool/testdata/fileonlyfixture"); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes)
	return root, bytes, toolpkg.ToolDef{
		Name: "fixture", Transport: toolpkg.TransportNativeFileOnly, Command: "fixture.exe",
		SHA256: hex.EncodeToString(sum[:]), PackageRoot: root,
		Actions: map[string]*toolpkg.ToolAction{"run": {
			Result: &schema.ActionResultContract{
				Format: schema.ActionResultFormatQueryResultV1, Source: schema.ActionResultSourceStdoutJSON,
				RowCount: "count", Columns: "cols", Rows: "items",
			},
		}},
	}
}

func resultStderr(result *toolpkg.ToolResult) string {
	if result == nil {
		return ""
	}
	return result.Stderr
}
