package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
)

// discover finds extensions from explicit declarations and .yawr/extensions.
func discover(ctx context.Context, manifest *extension.ProjectManifest, workdir string) ([]extension.ExtensionDecl, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if workdir == "" {
		workdir = "."
	}
	var decls []extension.ExtensionDecl
	seen := make(map[string]string)

	add := func(decl extension.ExtensionDecl) error {
		resolved, signature, err := resolveDeclWithSignature(workdir, decl)
		if err != nil {
			return err
		}
		if prior, exists := seen[resolved.Name]; exists {
			if prior != signature {
				return fmt.Errorf("conflicting extension id %q", resolved.Name)
			}
			return nil
		}
		seen[resolved.Name] = signature
		decls = append(decls, resolved)
		return nil
	}

	if manifest != nil {
		for _, decl := range manifest.Extensions {
			if err := add(decl); err != nil {
				return nil, err
			}
		}
	}

	workspaceDir := filepath.Join(workdir, ".yawr", "extensions")
	entries, err := os.ReadDir(workspaceDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := add(extension.ExtensionDecl{Path: filepath.Join(workspaceDir, entry.Name())}); err != nil {
			return nil, err
		}
	}
	return decls, nil
}

func resolveDecl(workdir string, decl extension.ExtensionDecl) (extension.ExtensionDecl, error) {
	resolved, _, err := resolveDeclWithSignature(workdir, decl)
	return resolved, err
}

func resolveDeclWithSignature(workdir string, decl extension.ExtensionDecl) (extension.ExtensionDecl, string, error) {
	if decl.Path == "" {
		return extension.ExtensionDecl{}, "", fmt.Errorf("extension decl missing path")
	}
	path := decl.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(workdir, path)
	}
	manifest, _, err := readManifestDir(path)
	if err != nil {
		return extension.ExtensionDecl{}, "", err
	}
	name := strings.TrimSpace(decl.Name)
	if name == "" {
		name = manifest.Name
	}
	if decl.Name != "" && decl.Name != manifest.Name {
		return extension.ExtensionDecl{}, "", fmt.Errorf("extension name mismatch: %s vs %s", decl.Name, manifest.Name)
	}
	entrypoint := strings.TrimSpace(decl.Entrypoint)
	if entrypoint == "" {
		entrypoint = manifest.Entrypoint
	}
	if entrypoint == "" {
		return extension.ExtensionDecl{}, "", fmt.Errorf("extension %s missing entrypoint", name)
	}
	normalized, err := json.Marshal(manifest)
	if err != nil {
		return extension.ExtensionDecl{}, "", err
	}
	resolved := extension.ExtensionDecl{
		Name:       name,
		Path:       path,
		Entrypoint: entrypoint,
		Grants:     decl.Grants,
	}
	return resolved, string(normalized) + "\n" + filepath.Clean(entrypoint), nil
}
