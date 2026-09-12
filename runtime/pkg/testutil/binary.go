package testutil

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
)

// BinaryName returns the executable file name for the current platform.
func BinaryName(name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return name + ".exe"
	}
	return name
}

// BuildGoBinary builds pkg at outputPath, adding the platform executable suffix when needed.
func BuildGoBinary(root, outputPath, pkg string) (string, error) {
	outputPath = filepath.Join(filepath.Dir(outputPath), BinaryName(filepath.Base(outputPath)))
	cmd := exec.Command("go", "build", "-o", outputPath, pkg)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s: %w\n%s", pkg, err, string(out))
	}
	return outputPath, nil
}
