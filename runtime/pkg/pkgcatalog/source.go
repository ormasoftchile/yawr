package pkgcatalog

import (
	"crypto/sha256"
	"fmt"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"io/fs"
	"os"
	"path/filepath"
)

func digestBytes(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

// Source supplies request-local file reads without changing binding semantics.
// Nil callbacks retain the execution catalog's ordinary filesystem behavior.
type Source struct {
	ReadFile     func(string) ([]byte, error)
	Stat         func(string) (fs.FileInfo, error)
	WalkDir      func(string, fs.WalkDirFunc) error
	MetadataOnly bool
}

func (s *Source) read(path string) ([]byte, error) {
	if s != nil && s.ReadFile != nil {
		return s.ReadFile(path)
	}
	return os.ReadFile(path)
}
func (s *Source) stat(path string) (fs.FileInfo, error) {
	if s != nil && s.Stat != nil {
		return s.Stat(path)
	}
	return os.Stat(path)
}
func (s *Source) walk(path string, fn fs.WalkDirFunc) error {
	if s != nil && s.WalkDir != nil {
		return s.WalkDir(path, fn)
	}
	return filepath.WalkDir(path, fn)
}
func (s *Source) parse(path string) (*schema.ToolDef, error) {
	data, err := s.read(path)
	if err != nil {
		return nil, err
	}
	return internaltool.ParseToolBytes(data, path, s != nil && s.MetadataOnly)
}
func (s *Source) runtime(def *schema.ToolDef) (toolpkg.ToolDef, error) {
	if s == nil || !s.MetadataOnly {
		return internaltool.RuntimeToolDef(def)
	}
	out := toolpkg.ToolDef{Name: def.Name, Source: "tool://" + def.Name, Actions: map[string]*toolpkg.ToolAction{}}
	for name, a := range def.Actions {
		if a == nil {
			continue
		}
		args := map[string]*toolpkg.ArgDef{}
		for name, f := range a.Args {
			if f != nil {
				args[name] = &toolpkg.ArgDef{Type: f.Type, Presentation: f.Presentation}
			}
		}
		out.Actions[name] = (&toolpkg.ToolAction{Args: args, Outputs: a.Outputs, Execute: a.Execute}).WithSchemaAction(a)
	}
	return out, nil
}
func (s *Source) digest(path string) (string, error) {
	data, err := s.read(path)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}
