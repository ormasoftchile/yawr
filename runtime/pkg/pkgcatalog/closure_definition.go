package pkgcatalog

import (
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// Definition returns an owned declaration and runtime definition from captured
// sources. Local aliases do not change the canonical definition identity.
func (c *DependencyClosure) Definition(documentID, name string) (toolpkg.BoundDefinition, error) {
	for _, document := range c.Documents {
		if document.ID != documentID {
			continue
		}
		for _, binding := range document.Bindings {
			if binding.Name != name {
				continue
			}
			definition := toolpkg.BoundDefinition{Runtime: binding.Def}
			if binding.Def.SourcePath == "" {
				definition.Declaration = toolpkg.SchemaFromRuntime(binding.Def)
			} else {
				path, err := c.snapshot.capturedPath(binding.Def.SourcePath)
				if err != nil {
					return toolpkg.BoundDefinition{}, err
				}
				captured, ok := c.snapshot.files[path]
				if !ok || captured.err != nil {
					return toolpkg.BoundDefinition{}, errkit.New("SCOPE-002", fmt.Sprintf("definition source for %q is not captured", name))
				}
				declaration, err := c.Catalog.source.parse(path)
				if err != nil {
					return toolpkg.BoundDefinition{}, err
				}
				definition.Declaration = declaration
				definition.SourceDigest = digestBytes(captured.data)
				definition.Runtime.Name = declaration.Name
				definition.Runtime.SourcePath = path
			}
			if binding.Def.PackageName != "" {
				for _, candidate := range c.Catalog.Packages {
					if candidate.Name == binding.Def.PackageName {
						definition.PackageVersion = candidate.Version
						break
					}
				}
				if definition.PackageVersion == "" {
					return toolpkg.BoundDefinition{}, errkit.New("SCOPE-002", fmt.Sprintf("definition package for %q is not captured", name))
				}
			}
			return toolpkg.CloneBoundDefinition(definition)
		}
		return toolpkg.BoundDefinition{}, errkit.New("SCOPE-001", fmt.Sprintf("%s has no local binding for %q", document.Path, name))
	}
	return toolpkg.BoundDefinition{}, errkit.New("SCOPE-002", "definition owner is not in the dependency closure")
}
