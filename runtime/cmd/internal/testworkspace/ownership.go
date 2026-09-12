package testworkspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const MarkerName = ".yawr-authoring-workspace-owner"

var ErrAlreadyOwned = errors.New("authoring workspace already owned")

func Claim(root, owner string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect authoring workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("authoring workspace is not a directory: %s", root)
	}

	marker := filepath.Join(root, MarkerName)
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrAlreadyOwned, root)
		}
		return fmt.Errorf("claim authoring workspace: %w", err)
	}
	if _, err := file.WriteString(owner + "\n"); err != nil {
		file.Close()
		os.Remove(marker)
		return fmt.Errorf("record authoring workspace owner: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close authoring workspace marker: %w", err)
	}
	return nil
}
