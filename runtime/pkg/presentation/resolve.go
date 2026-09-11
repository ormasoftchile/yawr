package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	runtimetool "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"gopkg.in/yaml.v3"
)

func DecodeRequest(r io.Reader) (Request, error) {
	return decodeRequestVersion(r, SchemaVersion)
}

func decodeRequestVersion(r io.Reader, version string) (Request, error) {
	var req Request
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(data) > MaxBytes || !utf8.Valid(data) {
		return req, errors.New("invalid-request")
	}
	if !closedRequest(data) {
		return req, errors.New("invalid-request")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		return req, errors.New("invalid-request")
	}
	// Required fields cannot be confused with their zero value.
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || len(raw) != 5 {
		return req, errors.New("invalid-request")
	}
	if req.SchemaVersion != version || req.RequestID == "" || req.Overlays == nil || len(req.Overlays) > 128 || !filepath.IsAbs(req.Context.ProjectRoot) || req.Context.Generation < 0 || req.Context.Generation > 9007199254740991 {
		return req, errors.New("invalid-request")
	}
	for _, p := range []string{req.Context.PackageRoot, req.Context.PackageMapPath, req.Context.EntrypointPath} {
		if p != "" && !filepath.IsAbs(p) {
			return req, errors.New("invalid-request")
		}
	}
	seen := map[string]Buffer{}
	keyForPath := pathKey
	if version == ExpressionSchemaVersion {
		// Expression resolution has no filesystem authority, including symlink
		// probes on remote paths. Its buffers are identified lexically.
		keyForPath = lexicalPathKey
	}
	for _, b := range append([]Buffer{req.Document}, req.Overlays...) {
		if !validateBufferWithPathKey(b, keyForPath) || !utf8.ValidString(b.Text) {
			return req, errors.New("invalid-request")
		}
		key := keyForPath(b.Path)
		if prior, ok := seen[key]; ok && (prior.Version != b.Version || prior.Text != b.Text || prior.URI != b.URI) {
			return req, errors.New("invalid-request")
		}
		seen[key] = b
	}
	if strings.HasPrefix(strings.ToLower(req.Document.URI), "untitled:") {
		rel, err := filepath.Rel(keyForPath(req.Context.ProjectRoot), keyForPath(req.Document.Path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return req, errors.New("invalid-request")
		}
	}
	return req, nil
}

func Resolve(req Request) Reply {
	return resolveBindingSnapshot(req, nil, nil)
}

// bindingSnapshot is shared by the finite presentation and authoring operations.
// It retains the exact metadata-only filesystem used by Build and BindFile.
type bindingSnapshot struct {
	ctx           context.Context
	files         *filesystem
	catalog       *pkgcatalog.Catalog
	root          *yaml.Node
	definitions   map[string]runtimetool.ToolDef
	catalogReason string
}

func resolveBindingSnapshot(req Request, sourceRoot *yaml.Node, snapshot *bindingSnapshot) Reply {
	reply := Reply{SchemaVersion: SchemaVersion, ResolverVersion: ResolverVersion, RequestID: req.RequestID, Context: req.Context, Document: Document{URI: req.Document.URI, Version: req.Document.Version}, Status: "resolved", Bindings: []Binding{}, Regions: []Region{}, Dependencies: []Dependency{}}
	f := &filesystem{buffers: map[string]Buffer{}, reads: map[string]fileSnapshot{}, deps: map[string]Dependency{}}
	if snapshot != nil {
		snapshot.files = f
		f.ctx = snapshot.ctx
		snapshot.definitions = map[string]runtimetool.ToolDef{}
	}
	for _, b := range append([]Buffer{req.Document}, req.Overlays...) {
		f.buffers[pathKey(b.Path)] = b
	}
	fail := func(reason string) Reply {
		reply.Status, reply.Reason = "unavailable", reason
		reply.Regions = []Region{}
		reply.Dependencies = f.dependencies()
		return reply
	}
	text, _ := f.read(req.Document.Path)
	root, complete := sourceRoot, true
	if root == nil {
		root, complete = parseSource(string(text))
	}
	if snapshot != nil {
		snapshot.root = root
	}
	if root == nil || !uniqueMappingKeys(root) {
		return fail("incomplete-source")
	}
	refsNode := mapValue(root, "toolRefs")
	if refsNode != nil && (refsNode.Kind != yaml.SequenceNode || !uniqueNodes(refsNode)) {
		return fail("incomplete-identity")
	}
	var refs []*schema.ToolRef
	if refsNode != nil && refsNode.Decode(&refs) != nil {
		return fail("incomplete-identity")
	}
	seen := map[string]bool{}
	for _, r := range refs {
		if r == nil || r.Name == "" || r.Alias != "" || seen[r.Name] {
			return fail("incomplete-identity")
		}
		seen[r.Name] = true
	}
	entry := req.Context.EntrypointPath
	if entry == "" {
		entry = req.Document.Path
	}
	entryRoot := root
	if pathKey(entry) != pathKey(req.Document.Path) {
		data, err := f.read(entry)
		if err != nil {
			return fail("missing-dependency")
		}
		entryRoot, _ = parseSource(string(data))
		if entryRoot == nil || !uniqueMappingKeys(entryRoot) {
			return fail("incomplete-identity")
		}
	}
	var requirements []*schema.PackageRequirement
	if n := mapValue(entryRoot, "requires"); n != nil && n.Decode(&requirements) != nil {
		return fail("incomplete-identity")
	}
	loadConfig := func(path string, optional bool) (*schema.ProjectConfig, error) {
		data, err := f.read(path)
		if optional && os.IsNotExist(err) {
			return &schema.ProjectConfig{}, nil
		}
		if err != nil {
			return nil, err
		}
		var n yaml.Node
		var cfg schema.ProjectConfig
		if yaml.Unmarshal(data, &n) != nil || !uniqueNodes(&n) || n.Decode(&cfg) != nil || cfg.APIVersion != schema.ProjectConfigAPIVersion {
			return nil, errors.New("invalid config")
		}
		return &cfg, nil
	}
	canonicalConfig := filepath.Join(req.Context.ProjectRoot, ".yawr", "config.yaml")
	configData, err := f.read(canonicalConfig)
	var cfg *schema.ProjectConfig
	if os.IsNotExist(err) {
		cfg = &schema.ProjectConfig{}
		err = nil
	} else if err == nil {
		var n yaml.Node
		cfg = &schema.ProjectConfig{}
		if yaml.Unmarshal(configData, &n) != nil || !uniqueNodes(&n) || n.Decode(cfg) != nil || cfg.APIVersion != schema.ProjectConfigAPIVersion {
			err = errors.New("invalid config")
		}
	}
	if err != nil {
		return fail("incomplete-identity")
	}
	if req.Context.PackageMapPath != "" {
		override, err := loadConfig(req.Context.PackageMapPath, false)
		if err != nil {
			return fail("incomplete-identity")
		}
		cfg.Requires, _ = pkgcatalog.MergePackageBindings(cfg.Requires, override.Requires)
		cfg.ToolPaths = append(cfg.ToolPaths, override.ToolPaths...)
	}
	catalogFiles := map[string]bool{}
	source := &pkgcatalog.Source{ReadFile: func(path string) ([]byte, error) { catalogFiles[pathKey(path)] = true; return f.read(path) }, Stat: f.stat, WalkDir: f.walk, MetadataOnly: true}
	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{Source: source, Builtins: internaltool.NewBuiltinRegistry().All(), WorkspaceRoot: req.Context.ProjectRoot, ProjectRequires: cfg.Requires, RunbookRequires: requirements, RunbookPath: entry, ProjectToolPaths: cfg.ToolPaths})
	fatal, _ := errkit.SplitWarnings(errs)
	catalogReason := ""
	if len(fatal) > 0 {
		catalogReason = reasonFor(fatal)
		reply.Status, reply.Reason = "unavailable", catalogReason
	}
	if snapshot != nil {
		snapshot.catalog, snapshot.catalogReason = catalog, catalogReason
	}
	if req.Context.PackageRoot != "" {
		if catalogReason != "" {
			return fail(catalogReason)
		}
		proven := false
		for _, e := range catalog.RunbookEntries() {
			if pathKey(e.PackageRoot) == pathKey(req.Context.PackageRoot) {
				rel, err := filepath.Rel(req.Context.PackageRoot, req.Document.Path)
				if err == nil {
					_, _, err = pkgpath.ResolveKind(pkgpath.KindPackageInternalInclude, filepath.Join(req.Context.PackageRoot, "_"), filepath.ToSlash(rel), req.Context.ProjectRoot, req.Context.PackageRoot)
				}
				if err == nil {
					proven = true
				}
			}
		}
		// Tool substitutions are package-owned only when their declared path
		// resolves to this document with the same containment root.
		for _, e := range catalog.Entries() {
			if pathKey(e.Def.PackageRoot) != pathKey(req.Context.PackageRoot) {
				continue
			}
			if catalogFiles[pathKey(req.Document.Path)] {
				proven = true
			}
			for _, a := range e.Def.Actions {
				if a.Execute == nil || !a.Execute.IsSubstitution() {
					continue
				}
				p, _, err := pkgpath.ResolveKind(pkgpath.KindExecutePath, e.SourcePath, a.Execute.Path, req.Context.ProjectRoot, e.Def.PackageRoot)
				if err == nil && pathKey(p) == pathKey(req.Document.Path) {
					proven = true
				}
			}
		}
		if !proven {
			return fail("incomplete-identity")
		}
	}
	for i, ref := range refs {
		b := Binding{ID: fmt.Sprintf("b%d", i), Name: ref.Name, Status: "unavailable", Reason: "missing-dependency", Actions: []Action{}}
		if catalogReason != "" && ref.Path == "" {
			b.Reason = catalogReason
			if catalogReason == "ambiguous-binding" {
				b.Status = "ambiguous"
			}
			reply.Bindings = append(reply.Bindings, b)
			continue
		}
		bound, errs := pkgcatalog.BindFile(catalog, req.Document.Path, []*schema.ToolRef{ref}, req.Context.PackageRoot)
		fatal, _ := errkit.SplitWarnings(errs)
		if len(fatal) > 0 || len(bound) != 1 {
			b.Reason = reasonFor(fatal)
			if b.Reason == "ambiguous-binding" {
				b.Status = "ambiguous"
			}
			reply.Bindings = append(reply.Bindings, b)
			continue
		}
		def := bound[0].Def
		if snapshot != nil {
			snapshot.definitions[b.ID] = def
		}
		b.Status, b.Reason, b.ToolID = "resolved", "", def.Name
		if def.SourcePath == "" {
			data, err := json.Marshal(def)
			if err != nil {
				b.Status, b.Reason = "unavailable", "missing-dependency"
			} else {
				b.ToolDigest = Digest(data)
			}
		} else {
			data, err := f.read(def.SourcePath)
			if err != nil {
				b.Status, b.Reason = "unavailable", "missing-dependency"
			} else {
				b.ToolDigest = Digest(data)
				b.SourceURI = FileURI(def.SourcePath)
				if overlay, ok := f.buffers[pathKey(def.SourcePath)]; ok {
					b.SourceURI = overlay.URI
				}
			}
		}
		if ref.Path == "" {
			for _, entry := range catalog.Entries() {
				if pathKey(entry.SourcePath) == pathKey(def.SourcePath) && entry.Digest != "" {
					b.ToolDigest = entry.Digest
					break
				}
			}
		}
		for name, a := range def.Actions {
			args := map[string]*schema.ArgDef{}
			for name, v := range a.Args {
				if v != nil {
					args[name] = &schema.ArgDef{Type: v.Type, Presentation: v.Presentation}
				}
			}
			b.Actions = append(b.Actions, Action{Name: name, Arguments: Fields(args), Outputs: Fields(a.Outputs)})
		}
		sort.Slice(b.Actions, func(i, j int) bool { return b.Actions[i].Name < b.Actions[j].Name })
		reply.Bindings = append(reply.Bindings, b)
	}
	if snapshot == nil {
		var invalidRange bool
		reply.Regions, invalidRange = collectRegions(req.Document.Text, root, reply.Bindings)
		if invalidRange {
			reply.Status, reply.Reason = "unavailable", "invalid-source-range"
		}
	}
	if !complete {
		reply.Status, reply.Reason = "unavailable", "incomplete-source"
	}
	sort.Slice(reply.Regions, func(i, j int) bool { return reply.Regions[i].Range.Start < reply.Regions[j].Range.Start })
	reply.Dependencies = f.dependencies()
	if len(reply.Bindings) > MaxEntries || len(reply.Regions) > MaxEntries {
		return fail("limit-exceeded")
	}
	if f.stale() {
		reply.Status, reply.Reason = "stale", "stale-request"
		reply.Bindings = []Binding{}
		reply.Regions = []Region{}
	}
	return reply
}

func reasonFor(errs []error) string {
	for _, err := range errs {
		msg := err.Error()
		if strings.Contains(msg, "PKG-006") || strings.Contains(msg, "PKG-022") {
			return "ambiguous-binding"
		}
		if strings.Contains(msg, "presentation") {
			return "invalid-descriptor"
		}
		if strings.Contains(msg, "limit-exceeded") {
			return "limit-exceeded"
		}
		if strings.Contains(msg, "identity") || strings.Contains(msg, "PKG-029") || strings.Contains(msg, "PKG-007") || strings.Contains(msg, "PKG-021") || strings.Contains(msg, "PKG-023") || strings.Contains(msg, "missing required name") {
			return "incomplete-identity"
		}
		if strings.Contains(msg, "invalid tool source") {
			return "incomplete-source"
		}
		if strings.Contains(msg, "parse") || strings.Contains(msg, "PKG-004") {
			return "incomplete-source"
		}
	}
	return "missing-dependency"
}
