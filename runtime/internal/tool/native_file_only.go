package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

const (
	fileOnlyTimeout     = 30 * time.Second
	fileOnlyOutputLimit = 1 << 20
)

// NativeFileOnlyTransport executes a package-relative, digest-pinned binary
// inside the Windows AppContainer boundary.
type NativeFileOnlyTransport struct{}

func (t *NativeFileOnlyTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	if !toolpkg.FileOnlySubprocessAvailable() {
		return nil, fmt.Errorf("native-file-only: Windows AMD64 enforcement backend unavailable")
	}
	actionDef, ok := def.Actions[action]
	if !ok {
		return nil, fmt.Errorf("native-file-only tool %s: action %q not found", def.Name, action)
	}
	if def.PackageRoot == "" {
		return nil, fmt.Errorf("native-file-only tool %s: package root is unavailable", def.Name)
	}
	renderedArgv, err := renderArgv(actionDef.Argv, args)
	if err != nil {
		return nil, fmt.Errorf("native-file-only tool %s action %s: render argv: %w", def.Name, action, err)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("native-file-only: locate cache: %w", err)
	}
	base := filepath.Join(cache, "yawr", "native-file-only")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("native-file-only: create sandbox base: %w", err)
	}
	sandbox, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return nil, fmt.Errorf("native-file-only: create sandbox: %w", err)
	}
	defer os.RemoveAll(sandbox)

	stagedCommand, digest, err := stagePackageFile(def.PackageRoot, def.Command, sandbox)
	if err != nil {
		return nil, fmt.Errorf("native-file-only: command: %w", err)
	}
	if digest != def.SHA256 {
		return nil, fmt.Errorf("native-file-only: executable digest mismatch: got %s", digest)
	}
	protectedCommand, err := protectStagedExecutable(stagedCommand, def.SHA256)
	if err != nil {
		return nil, fmt.Errorf("native-file-only: protect verified executable: %w", err)
	}
	defer protectedCommand.Close()
	for _, input := range def.Inputs {
		if _, _, err := stagePackageFile(def.PackageRoot, input, sandbox); err != nil {
			return nil, fmt.Errorf("native-file-only: input %q: %w", input, err)
		}
	}
	scratch := filepath.Join(sandbox, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("native-file-only: create scratch: %w", err)
	}

	boundedCtx, cancel := context.WithTimeout(ctx, fileOnlyTimeout)
	defer cancel()
	stdout, stderr, exitCode, err := executeFileOnly(boundedCtx, stagedCommand, renderedArgv, sandbox, fileOnlyOutputLimit)
	result := &toolpkg.ToolResult{ExitCode: exitCode, Stdout: string(stdout), Stderr: string(stderr)}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || boundedCtx.Err() != nil {
			return result, fmt.Errorf("native-file-only tool %s: %w", def.Name, boundedCtx.Err())
		}
		return result, fmt.Errorf("native-file-only tool %s: %w", def.Name, err)
	}
	if exitCode != 0 {
		return result, fmt.Errorf("native-file-only tool %s exited with code %d", def.Name, exitCode)
	}
	if actionDef.Result != nil {
		output, err := parseNativeActionResult(result.Stdout, actionDef.Result)
		if err != nil {
			return result, fmt.Errorf("native-file-only tool %s action %s: result: %w", def.Name, action, err)
		}
		result.Output = output
	}
	return result, nil
}

func stagePackageFile(root, relative, sandbox string) (string, string, error) {
	src, clean, err := openPackageRegularFile(root, relative)
	if err != nil {
		return "", "", err
	}
	defer src.Close()
	dst := filepath.Join(sandbox, clean)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", "", err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), src)
	closeErr := out.Close()
	if copyErr != nil {
		return "", "", copyErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	return dst, hex.EncodeToString(hash.Sum(nil)), nil
}

var protectStagedExecutable = func(path, expectedDigest string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenFileDigest(file, expectedDigest); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func verifyOpenFileDigest(file *os.File, expectedDigest string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != expectedDigest {
		return fmt.Errorf("executable digest mismatch after identity protection: got %s", digest)
	}
	return nil
}

func (t *NativeFileOnlyTransport) Close() error { return nil }
