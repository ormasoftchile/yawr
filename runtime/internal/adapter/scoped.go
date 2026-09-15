package adapter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/toolscope"
	"gopkg.in/yaml.v3"
)

// ScopedRunOptions supplies the effective configuration before any runtime or
// provider starts. Preparation never registers authored names globally.
type ScopedRunOptions struct {
	Catalog    pkgcatalog.BuildOptions
	Parser     parser.Parser
	Entrypoint string
	Profile    *schema.RuntimeProfile
}

type PreparedScopedRun struct {
	// Catalog exposes captured evidence metadata only. Execution resolves
	// exclusively against Scopes, never against this catalog's name tables.
	Catalog        *pkgcatalog.Catalog
	Root           *parser.ParsedRunbook
	Scopes         *toolscope.Set
	RootScopeID    string
	Loader         *ScopedRunbookLoader
	LazyLoader     *ScopedLazyLoader
	Resolver       *FrozenScopedIncludeResolver
	Profile        *schema.RuntimeProfile
	Warnings       []error
	packageDigests map[string]string
}

// PrepareScopedRun captures the dependency fixed point, seals lexical bindings,
// then materializes executable bodies against those stable identities. Returned
// loaders and resolvers never reopen source files or consult a name registry.
func PrepareScopedRun(ctx context.Context, options ScopedRunOptions) (*PreparedScopedRun, error) {
	if options.Catalog.Source != nil && options.Catalog.Source.MetadataOnly {
		return nil, errkit.New("SCOPE-002", "scoped execution requires complete runtime metadata, not a metadata-only catalog")
	}
	profile, err := captureScopedProfile(options.Profile)
	if err != nil {
		return nil, err
	}
	entrypointAlias := options.Entrypoint
	entrypointIdentity := options.Entrypoint
	if options.Catalog.Source == nil && filepath.IsAbs(options.Entrypoint) {
		canonical, err := filepath.EvalSymlinks(options.Entrypoint)
		if err != nil {
			return nil, err
		}
		entrypointIdentity = canonical
	}
	closure, issues := pkgcatalog.BuildClosure(ctx, pkgcatalog.ClosureOptions{
		Catalog: options.Catalog, Entrypoint: options.Entrypoint, Parser: options.Parser,
	})
	fatal, warnings := errkit.SplitWarnings(issues)
	if len(fatal) != 0 {
		return nil, errors.Join(fatal...)
	}
	if closure == nil || closure.Catalog == nil {
		return nil, fmt.Errorf("scoped preparation: dependency closure is missing")
	}
	snapshot := toolscope.Snapshot{
		Version: toolscope.Version, CatalogDigest: closure.Catalog.CatalogDigest(),
		ProfileDigest: toolscope.ProfileDigest(profile),
		Documents:     map[string]toolscope.Document{}, Scopes: map[string]toolscope.Scope{},
		Bindings: map[string]toolscope.Binding{}, Definitions: map[string]tool.BoundDefinition{},
		DynamicTargets: map[string]toolscope.Target{},
	}
	loader := &ScopedRunbookLoader{closure: closure, documents: map[string]*pkgcatalog.DependencyDocument{},
		owners: map[string][]string{}, discoveryScopes: map[string]string{},
		pathAliases: map[string]string{
			scopedPathKey(entrypointAlias):               entrypointIdentity,
			scopedPathKey(filepath.Dir(entrypointAlias)): filepath.Dir(entrypointIdentity),
		}}
	var rootScope string
	root, err := closure.Load(ctx, options.Entrypoint)
	if err != nil {
		return nil, err
	}
	for _, source := range closure.Documents {
		identity := source.PackageRoot
		if identity == "" {
			identity = "workspace:" + filepath.Clean(options.Catalog.WorkspaceRoot)
		}
		document := toolscope.Document{SourceIdentity: source.Path, SourceDigest: source.Digest, PackageIdentity: identity}
		id := toolscope.DocumentID(document)
		scopeID := toolscope.ScopeID(id, snapshot.CatalogDigest, snapshot.ProfileDigest)
		document.ScopeID = scopeID
		snapshot.Documents[id] = document
		snapshot.Scopes[scopeID] = toolscope.Scope{DocumentID: id, Bindings: map[string]string{}}
		loader.documents[scopeID] = source
		loader.discoveryScopes[source.ID] = scopeID
		key := scopedPathKey(source.Path)
		loader.owners[key] = append(loader.owners[key], scopeID)
		if scopedPathKey(entrypointIdentity) == key && source.PackageRoot == "" {
			rootScope = scopeID
		}
	}
	if rootScope == "" {
		return nil, fmt.Errorf("scoped preparation: entrypoint owner is missing")
	}
	for _, source := range closure.Documents {
		scopeID := loader.discoveryScopes[source.ID]
		scope := snapshot.Scopes[scopeID]
		document := snapshot.Documents[scope.DocumentID]
		for _, edge := range source.Includes {
			target, ok := snapshot.Scopes[loader.discoveryScopes[edge.Target]]
			if !ok {
				return nil, fmt.Errorf("scoped preparation: include target is missing")
			}
			document.StaticEdges = append(document.StaticEdges, toolscope.Edge{StepID: edge.StepID, DocumentID: target.DocumentID})
		}
		snapshot.Documents[scope.DocumentID] = document
		for _, binding := range source.Bindings {
			definition, err := closure.Definition(source.ID, binding.Name)
			if err != nil {
				return nil, err
			}
			if err := freezeScopedConfiguration(&definition, binding.Name, profile); err != nil {
				return nil, err
			}
			id, err := tool.DefinitionID(definition)
			if err != nil {
				return nil, err
			}
			bindingID := tool.BindingID(scopeID, binding.Name, id)
			scope.Bindings[binding.Name] = bindingID
			snapshot.Bindings[bindingID] = toolscope.Binding{ScopeID: scopeID, LogicalName: binding.Name,
				DefinitionID: id, DeclarationSite: source.Path, SelectionProvenance: definition.Runtime.SourcePath}
			snapshot.Definitions[id] = definition
		}
	}
	loader.scopes, err = toolscope.New(snapshot)
	if err != nil {
		return nil, err
	}
	if err := materializeScopedDefinitions(ctx, loader, &snapshot); err != nil {
		return nil, err
	}
	if err := captureScopedTargets(ctx, loader, &snapshot); err != nil {
		return nil, err
	}
	loader.scopes, err = toolscope.New(snapshot)
	if err != nil {
		return nil, err
	}
	if err := validateScopedBodies(loader.scopes); err != nil {
		return nil, err
	}
	root, err = loader.LoadScope(ctx, root.Source, rootScope)
	if err != nil {
		return nil, err
	}
	packageDigests := make(map[string]string, len(closure.Catalog.Packages))
	for _, pkg := range closure.Catalog.Packages {
		packageDigests[pkg.Name] = pkg.Digest
	}
	return &PreparedScopedRun{Catalog: closure.Catalog, Root: root, Scopes: loader.scopes, RootScopeID: rootScope, Loader: loader,
		LazyLoader: &ScopedLazyLoader{loader: loader},
		Resolver:   NewFrozenScopedIncludeResolver(), Profile: profile, Warnings: warnings, packageDigests: packageDigests}, nil
}

func captureScopedProfile(profile *schema.RuntimeProfile) (*schema.RuntimeProfile, error) {
	if profile == nil {
		return nil, nil
	}
	body, err := yaml.Marshal(profile)
	if err != nil {
		return nil, err
	}
	return schema.ParseProfileBytes(body)
}

func freezeScopedConfiguration(definition *tool.BoundDefinition, name string, profile *schema.RuntimeProfile) error {
	if definition.Declaration == nil || definition.Runtime.Name == "" {
		return fmt.Errorf("scoped preparation: %q has no captured declaration", name)
	}
	if definition.Runtime.Transport == "" || definition.Declaration.Transport.Type == "" {
		return errkit.New("SCOPE-002", fmt.Sprintf("scoped preparation: %q has no captured transport metadata", name))
	}
	for actionName, action := range definition.Runtime.Actions {
		if action == nil || definition.Declaration.Actions[actionName] == nil {
			return fmt.Errorf("scoped preparation: %q action %q has incomplete metadata", name, actionName)
		}
	}
	if profile == nil || profile.Tools[name] == nil {
		return nil
	}
	override := profile.Tools[name]
	if override.Mode != "" {
		return errkit.New("PROF-001", "scoped profile cannot rewrite a transport mode")
	}
	if override.Endpoint == "" && override.Provider == "" {
		return nil
	}
	cfg := &definition.Declaration.Transport
	if cfg.Type != schema.TransportMCPHTTP {
		return fmt.Errorf("scoped preparation: endpoint/provider override for non-mcp-http tool %q", name)
	}
	if cfg.Auth == nil {
		return errkit.New("PLAN-013", fmt.Sprintf("tool %q override requires captured auth.allowed_hosts", name))
	}
	if override.Endpoint != "" {
		if err := internaltool.ValidateAuthConfig(override.Endpoint, cfg.Auth); err != nil {
			return errkit.Wrap("PLAN-013", "profile endpoint is not permitted", err)
		}
		cfg.URL = override.Endpoint
	}
	if override.Provider != "" {
		cfg.Auth.Provider = override.Provider
	}
	if errs := internaltool.ValidateTransportConfig(*cfg); len(errs) != 0 {
		return errors.Join(errs...)
	}
	definition.Runtime.URL = cfg.URL
	definition.Runtime.Auth = cfg.Auth
	return nil
}
