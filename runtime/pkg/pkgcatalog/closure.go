package pkgcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/semver"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"gopkg.in/yaml.v3"
)

const (
	MaxClosureDocuments = 4096
	MaxClosurePackages  = 256
	MaxClosureBytes     = 64 * 1024 * 1024
)

type ClosureOptions struct {
	Catalog    BuildOptions
	Entrypoint string
	Parser     parser.Parser
}

type DependencyEdge struct {
	StepID string `json:"stepID"`
	Target string `json:"target"`
}

type DependencyDocument struct {
	// ID is a discovery key, not the versioned executable document identity.
	ID               string
	Path             string
	PackageRoot      string
	Digest           string
	Includes         []DependencyEdge
	Substitutions    []DependencyEdge
	Bindings         []Binding
	parsed           *parser.ParsedRunbook
	calls            []*schema.Step
	dynamic          bool
	implicitBuiltins map[string]bool
	includeHeight    int
}

type closureBindingRecord struct {
	Name          string
	Definition    toolpkg.ToolDef
	SchemaActions map[string]*schema.ToolAction
}

type closureDocumentRecord struct {
	ID            string
	Path          string
	PackageRoot   string
	Digest        string
	Includes      []DependencyEdge
	Substitutions []DependencyEdge
	Bindings      []closureBindingRecord
}

// DependencyClosure is preflight metadata, not authorization to execute.
// Engine adapters must seal scopes and validate an executable plan separately.
type DependencyClosure struct {
	Catalog   *Catalog
	Documents []*DependencyDocument
	Targets   map[string]string
	Digest    string
	snapshot  *closureSource
	parser    parser.Parser
}

// Load parses the captured bytes, never the current filesystem. Each caller
// receives an independent parse tree for planner-owned annotations.
func (c *DependencyClosure) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := c.SourceIdentity(path)
	if err != nil {
		return nil, err
	}
	captured := c.snapshot.files[key]
	parsed, err := c.parser.ParseBytes(ctx, append([]byte(nil), captured.data...))
	if err == nil && (parsed == nil || parsed.Runbook == nil) {
		return nil, errkit.New("SCOPE-002", "captured dependency source has no parsed runbook")
	}
	if err == nil {
		parsed.Source, err = filepath.Abs(path)
	}
	return parsed, err
}

// SourceIdentity resolves only aliases captured during preflight. It uses the
// same frozen identity as Load without consulting the current filesystem.
func (c *DependencyClosure) SourceIdentity(path string) (string, error) {
	key, err := c.snapshot.capturedPath(path)
	if err != nil {
		return "", err
	}
	captured, ok := c.snapshot.files[key]
	if !ok || captured.err != nil {
		return "", errkit.New("SCOPE-002", fmt.Sprintf("source %q is not in the frozen dependency closure", path))
	}
	return key, nil
}

type closureFile struct {
	data []byte
	err  error
}

type closureStat struct {
	info fs.FileInfo
	err  error
}

type closureWalkEntry struct {
	path  string
	entry fs.DirEntry
	err   error
}

type closureWalk struct {
	entries []closureWalkEntry
	err     error
}

type closureSource struct {
	ctx      context.Context
	base     *Source
	files    map[string]closureFile
	paths    map[string]string
	stats    map[string]closureStat
	walks    map[string]closureWalk
	bytes    int
	limitErr error
	sealed   bool
}

func (s *closureSource) capturedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	alias := pkgpath.NormalizeForComparison(filepath.Clean(abs), os.PathSeparator == '\\')
	key := s.paths[alias]
	for parent := filepath.Dir(alias); key == ""; parent = filepath.Dir(parent) {
		if capturedParent, ok := s.paths[parent]; ok {
			relative, err := filepath.Rel(parent, alias)
			if err != nil {
				return "", err
			}
			key = filepath.Join(capturedParent, relative)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return key, nil
}

func (s *closureSource) path(path string) (string, error) {
	if s.sealed {
		key, err := s.capturedPath(path)
		if err != nil {
			return "", err
		}
		if key == "" {
			return "", errkit.New("SCOPE-002", fmt.Sprintf("source %q is outside the frozen dependency closure", path))
		}
		return key, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	alias := pkgpath.NormalizeForComparison(filepath.Clean(abs), os.PathSeparator == '\\')
	if key, ok := s.paths[alias]; ok {
		return key, nil
	}
	key, err := closurePath(path)
	if err != nil {
		return "", err
	}
	if s.paths == nil {
		s.paths = map[string]string{}
	}
	s.paths[alias], s.paths[key] = key, key
	// Includes may address captured siblings through the entrypoint's short
	// directory spelling even when that directory is outside the workspace.
	if parent := filepath.Dir(abs); parent != abs {
		if _, err := s.path(parent); err != nil {
			return "", err
		}
	}
	return key, nil
}

func (s *closureSource) stat(path string) (fs.FileInfo, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	key, err := s.path(path)
	if err != nil {
		return nil, err
	}
	if cached, ok := s.stats[key]; ok {
		return cached.info, cached.err
	}
	if s.sealed {
		return nil, errkit.New("SCOPE-002", fmt.Sprintf("source metadata %q is not captured", path))
	}
	info, err := s.base.stat(key)
	if s.stats == nil {
		s.stats = map[string]closureStat{}
	}
	s.stats[key] = closureStat{info, err}
	return info, err
}

func (s *closureSource) walk(path string, fn fs.WalkDirFunc) error {
	key, err := s.path(path)
	if err != nil {
		return err
	}
	cached, ok := s.walks[key]
	if !ok {
		if s.sealed {
			return errkit.New("SCOPE-002", fmt.Sprintf("source directory %q is not captured", path))
		}
		cached.err = s.base.walk(key, func(path string, entry fs.DirEntry, err error) error {
			if ctxErr := s.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			cached.entries = append(cached.entries, closureWalkEntry{path, entry, err})
			return nil
		})
		if s.walks == nil {
			s.walks = map[string]closureWalk{}
		}
		s.walks[key] = cached
	}
	if cached.err != nil {
		return cached.err
	}
	skipDir := ""
	for _, item := range cached.entries {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if skipDir != "" && strings.HasPrefix(item.path, skipDir+string(filepath.Separator)) {
			continue
		}
		skipDir = ""
		err := fn(item.path, item.entry, item.err)
		if err == fs.SkipAll {
			return nil
		}
		if err == fs.SkipDir && item.entry != nil {
			skipDir = item.path
			if !item.entry.IsDir() {
				skipDir = filepath.Dir(item.path)
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func closurePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil && !os.IsNotExist(err) {
		return "", errkit.Wrap("PKG-008", "cannot resolve dependency source identity", err)
	}
	if err == nil {
		abs = resolved
	}
	return pkgpath.NormalizeForComparison(filepath.Clean(abs), os.PathSeparator == '\\'), nil
}

func (s *closureSource) read(path string) ([]byte, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if s.limitErr != nil {
		return nil, s.limitErr
	}
	key, err := s.path(path)
	if err != nil {
		return nil, err
	}
	if cached, ok := s.files[key]; ok {
		return append([]byte(nil), cached.data...), cached.err
	}
	if s.sealed {
		return nil, errkit.New("SCOPE-002", fmt.Sprintf("source %q is not captured", path))
	}
	if len(s.files) >= MaxClosureDocuments {
		s.limitErr = errkit.New("SCOPE-003", fmt.Sprintf("dependency closure exceeds %d captured source files", MaxClosureDocuments))
		return nil, s.limitErr
	}
	if info, err := s.stat(key); err == nil && info.Size() > int64(MaxClosureBytes-s.bytes) {
		s.limitErr = errkit.New("SCOPE-003", fmt.Sprintf("dependency closure exceeds %d source bytes", MaxClosureBytes))
		return nil, s.limitErr
	}
	data, readErr := s.base.read(key)
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > MaxClosureBytes-s.bytes {
		s.limitErr = errkit.New("SCOPE-003", fmt.Sprintf("dependency closure exceeds %d source bytes", MaxClosureBytes))
		return nil, s.limitErr
	}
	s.bytes += len(data)
	s.files[key] = closureFile{data: append([]byte(nil), data...), err: readErr}
	return data, readErr
}

type closureBuilder struct {
	ctx               context.Context
	options           ClosureOptions
	source            *Source
	snapshot          *closureSource
	documents         map[string]*DependencyDocument
	targets           map[string]string
	warnings          []error
	discoveryWarnings []error
	dynamic           bool
}

// BuildClosure resolves every eligible file to a fixed point. It performs no
// executor/provider operations and publishes no partial result on failure.
func BuildClosure(ctx context.Context, options ClosureOptions) (*DependencyClosure, []error) {
	if err := ctx.Err(); err != nil {
		return nil, []error{err}
	}
	if options.Parser == nil || !filepath.IsAbs(options.Entrypoint) || !filepath.IsAbs(options.Catalog.WorkspaceRoot) {
		return nil, []error{errkit.New("SCOPE-002", "dependency preflight requires a parser and absolute entrypoint/workspace paths")}
	}
	snapshot := &closureSource{ctx: ctx, base: options.Catalog.Source, files: map[string]closureFile{}}
	if _, err := snapshot.path(options.Catalog.WorkspaceRoot); err != nil {
		return nil, []error{err}
	}
	source := &Source{ReadFile: snapshot.read, Stat: snapshot.stat, WalkDir: snapshot.walk,
		MetadataOnly: snapshot.base != nil && snapshot.base.MetadataOnly}
	builder := &closureBuilder{ctx: ctx, options: options, source: source, snapshot: snapshot,
		documents: map[string]*DependencyDocument{}, targets: map[string]string{}}
	if _, err := builder.visit(options.Entrypoint, "", nil); err != nil {
		return nil, []error{err}
	}
	var catalog *Catalog
	var lastErrors []error
	packageSources := map[string]string{}
	bindingSources := map[string]string{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, []error{err}
		}
		before := len(builder.documents)
		opts := options.Catalog
		opts.Source, opts.LexicalBindings = source, true
		opts.RunbookRequires = nil
		opts.RequirementDocuments = builder.requirements()
		requirements, errs := ResolveRequirements(opts, opts.RequirementDocuments)
		if len(errs) != 0 {
			return nil, errs
		}
		if len(requirements) > MaxClosurePackages {
			return nil, []error{errkit.New("SCOPE-003", fmt.Sprintf("dependency closure exceeds %d packages", MaxClosurePackages))}
		}
		for _, requirement := range requirements {
			if requirement.ResolvedPath == "" {
				continue
			}
			name := requirement.Requirement.Package
			resolved := pkgpath.NormalizeForComparison(requirement.ResolvedPath, os.PathSeparator == '\\')
			if prior, ok := packageSources[name]; ok && prior != resolved {
				return nil, []error{errkit.New("PKG-024", fmt.Sprintf("package %s source changed during dependency preflight", name))}
			}
			packageSources[name] = resolved
		}
		catalog, errs = Build(opts)
		if snapshot.limitErr != nil {
			return nil, []error{snapshot.limitErr}
		}
		lastErrors, builder.warnings = errkit.SplitWarnings(errs)
		for _, err := range lastErrors {
			// A source binding can be supplied by another discovered file.
			// Other catalog failures must not be interpreted as an empty catalog.
			var coded errkit.Coder
			if !errors.As(err, &coded) || coded.Code() != "PKG-001" {
				return nil, lastErrors
			}
		}
		if builder.dynamic {
			for _, entry := range catalog.RunbookEntries() {
				doc, err := builder.visit(entry.Path, entry.PackageRoot, nil)
				if err != nil {
					return nil, []error{err}
				}
				if prior, ok := builder.targets[entry.Qualified]; ok && prior != doc.ID {
					return nil, []error{errkit.New("SCOPE-002", fmt.Sprintf("dynamic identity %q has multiple owners", entry.Qualified))}
				}
				builder.targets[entry.Qualified] = doc.ID
			}
		}
		var bindingErrors []error
		for _, document := range builder.sortedDocuments() {
			document.Bindings, document.Substitutions = nil, nil
			document.implicitBuiltins = map[string]bool{}
			provisional, err := builder.bindDocument(catalog, document)
			if err != nil {
				return nil, []error{err}
			}
			bindingErrors = append(bindingErrors, provisional...)
			for _, binding := range document.Bindings {
				key := document.ID + "\x00" + binding.Name
				source := pkgpath.NormalizeForComparison(binding.Def.SourcePath, os.PathSeparator == '\\')
				if prior, ok := bindingSources[key]; ok && prior != source {
					return nil, []error{errkit.New("SCOPE-002", fmt.Sprintf(
						"%s tool %s source changed during dependency preflight", document.Path, binding.Name))}
				}
				bindingSources[key] = source
				if err := builder.substitutions(document, binding); err != nil {
					return nil, []error{err}
				}
			}
		}
		if before == len(builder.documents) {
			lastErrors = append(lastErrors, bindingErrors...)
			break
		}
	}
	if len(lastErrors) != 0 {
		return nil, lastErrors
	}
	if err := builder.validateAncestorAliases(catalog); err != nil {
		return nil, []error{err}
	}
	closure := &DependencyClosure{Catalog: catalog, Documents: builder.sortedDocuments(),
		Targets: builder.targets, snapshot: snapshot, parser: options.Parser}
	record := struct {
		Catalog   string
		Sources   map[string]string
		Targets   map[string]string
		Documents []closureDocumentRecord
	}{Catalog: catalog.CatalogDigest(), Sources: map[string]string{}, Targets: closure.Targets}
	for _, document := range closure.Documents {
		item := closureDocumentRecord{ID: document.ID, Path: document.Path, PackageRoot: document.PackageRoot,
			Digest: document.Digest, Includes: document.Includes, Substitutions: document.Substitutions}
		for _, binding := range document.Bindings {
			frozen := closureBindingRecord{Name: binding.Name, Definition: binding.Def, SchemaActions: map[string]*schema.ToolAction{}}
			for name, action := range binding.Def.Actions {
				if declaration := action.SchemaAction(); declaration != nil {
					frozen.SchemaActions[name] = declaration
				}
			}
			item.Bindings = append(item.Bindings, frozen)
		}
		record.Documents = append(record.Documents, item)
	}
	for path, file := range snapshot.files {
		if file.err == nil {
			record.Sources[path] = digestBytes(file.data)
		}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, []error{err}
	}
	if err := ctx.Err(); err != nil {
		return nil, []error{err}
	}
	closure.Digest = digestBytes(body)
	snapshot.sealed = true
	return closure, append(builder.discoveryWarnings, builder.warnings...)
}

func (b *closureBuilder) sortedDocuments() []*DependencyDocument {
	keys := make([]string, 0, len(b.documents))
	for key := range b.documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	docs := make([]*DependencyDocument, 0, len(keys))
	for _, key := range keys {
		docs = append(docs, b.documents[key])
	}
	return docs
}

func (b *closureBuilder) requirements() []RequirementDocument {
	out := []RequirementDocument{}
	for _, document := range b.sortedDocuments() {
		out = append(out, RequirementDocument{Path: document.Path, PackageRoot: document.PackageRoot,
			Requirements: document.parsed.Runbook.Requires, Locations: requirementLocations(b.snapshot.files[document.Path].data)})
	}
	return out
}

func (b *closureBuilder) visit(path, packageRoot string, ancestors []string) (*DependencyDocument, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	path, err := b.snapshot.path(path)
	if err != nil {
		return nil, err
	}
	if packageRoot != "" {
		packageRoot, err = closurePath(packageRoot)
		if err != nil {
			return nil, err
		}
	}
	for _, prior := range ancestors {
		if prior == path {
			return nil, fmt.Errorf("%w: %s", flowwalk.ErrIncludeCycle, strings.Join(append(ancestors, path), " -> "))
		}
	}
	if len(ancestors) > flowwalk.DefaultMaxIncludeDepth {
		return nil, fmt.Errorf("%w: %s", flowwalk.ErrMaxDepthExceeded, path)
	}
	key := digestBytes([]byte(path + "\x00" + packageRoot))
	if doc := b.documents[key]; doc != nil {
		if len(ancestors)+doc.includeHeight > flowwalk.DefaultMaxIncludeDepth {
			return nil, fmt.Errorf("%w: %s", flowwalk.ErrMaxDepthExceeded, path)
		}
		return doc, nil
	}
	if len(b.documents) >= MaxClosureDocuments {
		return nil, errkit.New("SCOPE-003", fmt.Sprintf("dependency closure exceeds %d documents", MaxClosureDocuments))
	}
	data, err := b.source.read(path)
	if err != nil {
		return nil, fmt.Errorf("dependency source %s: %w", path, err)
	}
	parsed, err := b.options.Parser.ParseBytes(b.ctx, data)
	if err != nil {
		return nil, fmt.Errorf("dependency source %s: %w", path, err)
	}
	if parsed == nil || parsed.Runbook == nil {
		return nil, errkit.New("SCOPE-002", fmt.Sprintf("dependency source %s has no parsed runbook", path))
	}
	parsed.Source = path
	doc := &DependencyDocument{ID: key, Path: path, PackageRoot: packageRoot, Digest: digestBytes(data), parsed: parsed}
	b.documents[key] = doc
	visitor := &closureVisitor{builder: b, document: doc, ancestors: append(append([]string(nil), ancestors...), path)}
	if err := (&flowwalk.Walker{}).Walk(b.ctx, parsed, visitor); err != nil {
		return nil, err
	}
	return doc, nil
}

type closureVisitor struct {
	flowwalk.Base
	builder   *closureBuilder
	document  *DependencyDocument
	ancestors []string
}

func (v *closureVisitor) EnterStep(_ flowwalk.Ctx, step *schema.Step) error {
	if step.ToolCall != nil {
		v.document.calls = append(v.document.calls, step)
	}
	return v.builder.ctx.Err()
}

func (v *closureVisitor) BeforeInclude(ctx flowwalk.Ctx, step *schema.Step) (bool, error) {
	if step.IncludeSpec == nil {
		return false, errkit.New("SCOPE-002", "include has no source declaration")
	}
	if step.IncludeSpec.Include.IsDynamic() {
		v.document.dynamic, v.builder.dynamic = true, true
		return false, nil
	}
	authored := step.IncludeSpec.Include.Runbook
	if alias, ok := ctx.Imports[authored]; ok {
		authored = alias
	}
	kind := pkgpath.KindRequiresRunbook
	if v.document.PackageRoot != "" {
		kind = pkgpath.KindPackageInternalInclude
	}
	if err := pkgpath.ValidateAuthoredSyntax(authored, kind.AllowsAbsolute()); err != nil {
		return false, err
	}
	path, external, err := pkgpath.ResolveKind(kind, v.document.Path, authored, v.builder.options.Catalog.WorkspaceRoot, v.document.PackageRoot)
	if err != nil {
		return false, err
	}
	if external {
		v.builder.discoveryWarnings = append(v.builder.discoveryWarnings, errkit.New("PKG-W003", fmt.Sprintf("include %s in %s resolves outside the workspace", step.ID, v.document.Path)))
	}
	child, err := v.builder.visit(path, v.document.PackageRoot, v.ancestors)
	if err != nil {
		return false, err
	}
	v.document.Includes = append(v.document.Includes, DependencyEdge{StepID: step.ID, Target: child.ID})
	v.document.includeHeight = max(v.document.includeHeight, child.includeHeight+1)
	return false, nil
}

// Keep YAML declaration positions available to callers without making parser
// annotations part of an executable identity.
func requirementLocations(data []byte) []RequirementLocation {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) != 1 {
		return nil
	}
	mapping := root.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "requires" {
			continue
		}
		out := []RequirementLocation{}
		for _, item := range mapping.Content[i+1].Content {
			out = append(out, RequirementLocation{Line: item.Line, Column: item.Column})
		}
		return out
	}
	return nil
}

func (b *closureBuilder) bindDocument(catalog *Catalog, doc *DependencyDocument) ([]error, error) {
	bound := map[string]bool{}
	var provisional []error
	for _, ref := range doc.parsed.Runbook.ToolRefs {
		if ref == nil || ref.Name == "" || bound[ref.Name] {
			return nil, errkit.New("SCOPE-002", fmt.Sprintf("%s: missing or duplicate local tool name", doc.Path))
		}
		bound[ref.Name] = true
		if ref.Version != "" {
			if _, err := semver.ParseConstraint(ref.Version); err != nil {
				return nil, fmt.Errorf("%s toolRefs[%s]: %w", doc.Path, ref.Name, err)
			}
		}
		bindings, errs := BindFile(catalog, doc.Path, []*schema.ToolRef{ref}, doc.PackageRoot)
		fatal, warnings := errkit.SplitWarnings(errs)
		b.warnings = append(b.warnings, warnings...)
		for _, err := range fatal {
			var coded errkit.Coder
			missing := ref.Path == "" && errors.As(err, &coded) &&
				(coded.Code() == "PKG-011" || coded.Code() == "PKG-030")
			if !missing {
				return nil, fmt.Errorf("%s: %w", doc.Path, err)
			}
			provisional = append(provisional, fmt.Errorf("%s: %w", doc.Path, err))
		}
		doc.Bindings = append(doc.Bindings, bindings...)
	}
	rootPath, err := closurePath(b.options.Entrypoint)
	if err != nil {
		return nil, err
	}
	for _, step := range doc.calls {
		name := step.ToolCall.Tool.Name
		if bound[name] {
			continue
		}
		var binding *Binding
		if strings.Contains(name, "/") {
			if entry, ok := catalog.ByQualified(name); ok {
				def := entry.Def
				def.Name = name
				binding = &Binding{Name: name, Def: def}
			}
		} else if doc.Path == rootPath && doc.PackageRoot == "" {
			binding, err = bindByBareName(catalog, catalog.byBare, &schema.ToolRef{Name: name})
			if err != nil {
				var coded errkit.Coder
				if !errors.As(err, &coded) || coded.Code() != "PKG-030" {
					return nil, fmt.Errorf("%s step %s: %w", doc.Path, step.ID, err)
				}
			}
		} else {
			for _, candidate := range catalog.ByBare(name) {
				if candidate.Tier == TierBuiltin {
					binding = &Binding{Name: name, Def: candidate.Def}
					doc.implicitBuiltins[name] = true
					break
				}
			}
		}
		if binding == nil {
			provisional = append(provisional, errkit.New("SCOPE-001", fmt.Sprintf(
				"%s step %s: tool %q has no local binding; declare toolRefs: [{name: %q, package: <package>}] or a local path; parent aliases are not inherited",
				doc.Path, step.ID, name, name)))
			continue
		}
		doc.Bindings = append(doc.Bindings, *binding)
		bound[name] = true
	}
	sort.Slice(doc.Bindings, func(i, j int) bool { return doc.Bindings[i].Name < doc.Bindings[j].Name })
	return provisional, nil
}

func (b *closureBuilder) substitutions(doc *DependencyDocument, binding Binding) error {
	names := make([]string, 0, len(binding.Def.Actions))
	for name := range binding.Def.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		action := binding.Def.Actions[name]
		if action == nil || action.Execute == nil || !action.Execute.IsSubstitution() {
			continue
		}
		if binding.Def.SourcePath == "" || binding.Def.PackageRoot == "" {
			return errkit.New("SCOPE-002", fmt.Sprintf("%s tool %s action %s: substitution has no frozen source owner", doc.Path, binding.Name, name))
		}
		authored := action.Execute.Path
		if err := pkgpath.ValidateAuthoredSyntax(authored, pkgpath.KindExecutePath.AllowsAbsolute()); err != nil {
			return err
		}
		path, _, err := pkgpath.ResolveKind(pkgpath.KindExecutePath, binding.Def.SourcePath, authored,
			b.options.Catalog.WorkspaceRoot, binding.Def.PackageRoot)
		if err != nil {
			return err
		}
		child, err := b.visit(path, binding.Def.PackageRoot, nil)
		if err != nil {
			return err
		}
		doc.Substitutions = append(doc.Substitutions, DependencyEdge{StepID: binding.Name + "." + name, Target: child.ID})
	}
	return nil
}

func (b *closureBuilder) validateAncestorAliases(catalog *Catalog) error {
	for _, ancestor := range b.sortedDocuments() {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		visited := map[string]bool{ancestor.ID: true}
		var descendants func(*DependencyDocument) error
		descendants = func(parent *DependencyDocument) error {
			if err := b.ctx.Err(); err != nil {
				return err
			}
			targets := []string{}
			for _, edge := range append(append([]DependencyEdge(nil), parent.Includes...), parent.Substitutions...) {
				targets = append(targets, edge.Target)
			}
			if parent.dynamic {
				for _, target := range b.targets {
					targets = append(targets, target)
				}
			}
			sort.Strings(targets)
			for _, id := range targets {
				if visited[id] {
					continue
				}
				visited[id] = true
				child := b.documents[id]
				for _, binding := range ancestor.Bindings {
					if !child.implicitBuiltins[binding.Name] {
						continue
					}
					var builtin toolpkg.ToolDef
					for _, entry := range catalog.ByBare(binding.Name) {
						if entry.Tier == TierBuiltin {
							builtin = entry.Def
							break
						}
					}
					actual, err := json.Marshal(binding.Def)
					if err != nil {
						return err
					}
					expected, err := json.Marshal(builtin)
					if err != nil {
						return err
					}
					if string(actual) != string(expected) {
						return errkit.New("SCOPE-001", fmt.Sprintf(
							"%s: implicit built-in %q would change the ancestor binding in %s; declare a local toolRefs binding explicitly",
							child.Path, binding.Name, ancestor.Path))
					}
				}
				if err := descendants(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := descendants(ancestor); err != nil {
			return err
		}
	}
	return nil
}
