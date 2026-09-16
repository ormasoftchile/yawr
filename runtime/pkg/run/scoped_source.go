package run

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
)

func embeddedCatalogSource(root string, filesystem fs.FS) *pkgcatalog.Source {
	nameFor := func(path string) (string, error) {
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return "", fmt.Errorf("run: embedded dependency %q escapes its filesystem", path)
		}
		name := filepath.ToSlash(relative)
		if !fs.ValidPath(name) {
			return "", fmt.Errorf("run: invalid embedded dependency path %q", path)
		}
		return name, nil
	}
	return &pkgcatalog.Source{
		ReadFile: func(path string) ([]byte, error) {
			name, err := nameFor(path)
			if err != nil {
				return nil, err
			}
			return fs.ReadFile(filesystem, name)
		},
		Stat: func(path string) (fs.FileInfo, error) {
			name, err := nameFor(path)
			if err != nil {
				return nil, err
			}
			return fs.Stat(filesystem, name)
		},
		WalkDir: func(path string, visit fs.WalkDirFunc) error {
			name, err := nameFor(path)
			if err != nil {
				return err
			}
			return fs.WalkDir(filesystem, name, func(name string, entry fs.DirEntry, err error) error {
				return visit(filepath.Join(root, filepath.FromSlash(name)), entry, err)
			})
		},
	}
}
