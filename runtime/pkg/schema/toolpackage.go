package schema

// PackageMeta is the identity block of a Yawr tool-package manifest
// (yawr-package.yaml). The package root is the directory containing
// yawr-package.yaml.
type PackageMeta struct {
	Name        string `yaml:"name"                  json:"name"`
	Version     string `yaml:"version"               json:"version"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	License     string `yaml:"license,omitempty"     json:"license,omitempty"`
}

// ToolExport is a single exported tool entry under exports.tools[].
// The qualified catalog name for this export is "<meta.name>/<id>".
type ToolExport struct {
	ID   string `yaml:"id"   json:"id"`
	Path string `yaml:"path" json:"path"`
}

// RunbookExport is a single exported runbook entry under
// exports.runbooks[]. The qualified catalog identity for this export is
// "<meta.name>/<id>". Exported runbooks are the *only* targets a dynamic
// include (include.runbook_ref + resolve_from: catalog) may resolve to.
type RunbookExport struct {
	ID   string `yaml:"id"   json:"id"`
	Path string `yaml:"path" json:"path"`
}

// PackageExports is the package's public API surface (exports:).
type PackageExports struct {
	Tools    []ToolExport    `yaml:"tools"               json:"tools"`
	Runbooks []RunbookExport `yaml:"runbooks,omitempty"  json:"runbooks,omitempty"`
}

// PackageManifest is the parsed representation of a yawr-package.yaml file.
type PackageManifest struct {
	Schema       string         `yaml:"$schema,omitempty" json:"$schema,omitempty"`
	APIVersion   string         `yaml:"apiVersion"        json:"apiVersion"`
	Meta         PackageMeta    `yaml:"meta"              json:"meta"`
	Exports      PackageExports `yaml:"exports"           json:"exports"`
	Dependencies []any          `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
}

// PackageAPIVersion is the required apiVersion discriminator for a tool
// package manifest.
const PackageAPIVersion = "yawr.tool-package/v1"

// PackageManifestFilename is the canonical filename of a Yawr tool-package
// manifest. The package root is, by definition, the directory that contains
// this file. Both the requires:-driven catalog resolver (pkg/pkgcatalog) and
// the directory-scan tool registry (internal/tool) key their notion of
// "package root" off this single constant so the two paths agree.
const PackageManifestFilename = "yawr-package.yaml"
