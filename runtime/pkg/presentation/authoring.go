package presentation

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgpath"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	runtimetool "github.com/ormasoftchile/yawr/runtime/pkg/tool"
	"gopkg.in/yaml.v3"
)

func (r *AuthoringReply) unavailable(reason string) {
	if reason == "invalid-descriptor" {
		reason = "incomplete-identity"
	}
	r.Status, r.Reason = "unavailable", reason
	r.Items = []AuthoringItem{}
	r.Signature = nil
	r.RequiredEdit = nil
}

func ResolveAuthoring(ctx context.Context, req AuthoringRequest) (reply AuthoringReply) {
	reply = AuthoringReply{SchemaVersion: "authoring-reply/v3", ResolverVersion: AuthoringResolverVersion, GrammarVersion: "yawr-expression/v2",
		Operation: req.Operation, RequestID: req.RequestID, Context: req.Context,
		Document: AuthoringDocument{URI: req.Document.URI, Version: req.Document.Version, Digest: Digest([]byte(req.Document.Text))},
		Status:   "resolved", Discovery: AuthoringDiscovery{Scope: "explicit-local-catalog", Status: "not-needed"},
		Dependencies: []Dependency{}, Items: []AuthoringItem{}}
	reply.contextJSON = req.contextJSON
	snapshot := bindingSnapshot{ctx: ctx}
	defer func() {
		if snapshot.files != nil {
			reply.Dependencies = snapshot.files.dependencies()
			if snapshot.files.stale() {
				reply.unavailable("stale-request")
				reply.Status = "stale"
			}
		}
		if ctx.Err() != nil {
			reply.unavailable("stale-request")
			reply.Status = "stale"
		}
		sort.Slice(reply.Items, func(i, j int) bool {
			if reply.Items[i].Name != reply.Items[j].Name {
				return reply.Items[i].Name < reply.Items[j].Name
			}
			return reply.Items[i].ID < reply.Items[j].ID
		})
		data, err := json.Marshal(reply)
		if err != nil || len(data) > MaxBytes || len(reply.Items) > MaxEntries || len(reply.Dependencies) > MaxEntries {
			reply.unavailable("limit-exceeded")
			reply.Dependencies = []Dependency{}
		}
	}()
	caret, ok := authoringByteOffset(req.Document.Text, req.Position)
	if !ok {
		reply.unavailable("invalid-request")
		return
	}
	source, reason := parseAuthoringSource(req.Document.Text, caret, true)
	if reason != "" {
		reply.unavailable(reason)
		return
	}
	source.typed = true
	target := source.target(ctx, req.Operation)
	if source.unsafe {
		reply.unavailable("incomplete-source")
		return
	}
	if target == nil {
		if source.incomplete {
			reply.unavailable("incomplete-source")
		}
		return
	}
	if target.site.Kind == "expression" {
		if req.Operation != "required-arguments" {
			resolveAuthoringExpression(ctx, source, target, &reply)
		}
		return
	}
	if req.Operation == "signature" {
		return
	}
	reply.Site = &target.site
	if strings.HasPrefix(target.site.Kind, "include-") {
		completeInclude(source, target, &reply)
		if target.typedSchemaTarget {
			reply.Site.Kind = strings.Replace(reply.Site.Kind, "include-", "typed-", 1)
			for i := range reply.Items {
				reply.Items[i].Kind = strings.Replace(reply.Items[i].Kind, "include-", "typed-", 1)
				reply.Items[i].ID = strings.Replace(reply.Items[i].ID, "include-", "typed-", 1)
			}
		}
		return
	}
	if target.site.Kind == "argument" && target.args != nil && target.args.Style&yaml.FlowStyle != 0 {
		reply.unavailable("unsupported-context")
		return
	}
	root := source.root
	// The incomplete declaration is a candidate slot, not an existing binding.
	// All other lexical refs remain unchanged and validated by the shared loader.
	if target.ref != nil {
		copy := *root
		copy.Content = append([]*yaml.Node(nil), root.Content...)
		for i := 0; i < len(copy.Content); i += 2 {
			if copy.Content[i].Value == "toolRefs" {
				refs := *copy.Content[i+1]
				refs.Content = nil
				for _, ref := range copy.Content[i+1].Content {
					if ref != target.ref {
						refs.Content = append(refs.Content, ref)
					}
				}
				copy.Content[i+1] = &refs
			}
		}
		root = &copy
	}
	base := Request{SchemaVersion: SchemaVersion, RequestID: req.RequestID, Context: req.Context, Document: req.Document, Overlays: req.Overlays}
	resolved := resolveBindingSnapshot(base, root, &snapshot)
	reply.Discovery.Status = "complete"
	if snapshot.catalogReason != "" {
		reply.Discovery.Status, reply.Discovery.Reason = "limited", snapshot.catalogReason
		if reply.Discovery.Reason == "invalid-descriptor" {
			reply.Discovery.Reason = "incomplete-identity"
		}
	}
	if snapshot.catalog == nil || resolved.Status == "stale" || (resolved.Status != "resolved" && snapshot.catalogReason == "") {
		reason := resolved.Reason
		if reason == "" {
			reason = "incomplete-identity"
		}
		reply.Discovery.Status, reply.Discovery.Reason = "unavailable", reason
		reply.unavailable(reason)
		return
	}
	if target.ref != nil {
		if req.Operation == "complete" {
			completeToolReference(ctx, req, source, target, &snapshot, &reply)
		} else {
			reply.unavailable("unsupported-context")
		}
		return
	}
	if req.Operation == "complete" && target.site.Kind == "tool" {
		if dynamicName(target.node) {
			reply.unavailable("unresolved-dynamic")
			return
		}
		for _, b := range resolved.Bindings {
			if b.Status != "resolved" {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			edit, ok := source.nameEdit(target, b.Name)
			if !ok {
				reply.unavailable("invalid-source-range")
				return
			}
			item := AuthoringItem{ID: "tool:" + b.ID, Kind: "tool", Name: b.Name, Edit: edit}
			if metadata, ok := boundAuthoringMetadata(&snapshot, b.ID); ok {
				item.Description = metadata.Description
			}
			reply.Items = append(reply.Items, item)
		}
		return
	}
	nameNode := mapValue(target.tool, "name")
	if dynamicName(nameNode) {
		reply.unavailable("unresolved-dynamic")
		return
	}
	name := stringValue(nameNode)
	if name == "" {
		reply.unavailable("incomplete-identity")
		return
	}
	var binding *Binding
	for i := range resolved.Bindings {
		if resolved.Bindings[i].Name == name {
			binding = &resolved.Bindings[i]
			break
		}
	}
	if binding == nil {
		reply.unavailable("missing-dependency")
		return
	}
	if binding.Status != "resolved" {
		reply.unavailable(binding.Reason)
		return
	}
	reply.Site.BindingID, reply.Site.ToolID, reply.Site.ToolDigest = binding.ID, binding.ToolID, binding.ToolDigest
	metadata, ok := boundAuthoringMetadata(&snapshot, binding.ID)
	if !ok {
		reply.unavailable("incomplete-identity")
		return
	}
	if req.Operation == "complete" && target.site.Kind == "action" {
		if dynamicName(target.node) {
			reply.unavailable("unresolved-dynamic")
			return
		}
		for name, action := range metadata.Actions {
			edit, ok := source.nameEdit(target, name)
			if !ok {
				reply.unavailable("invalid-source-range")
				return
			}
			reply.Items = append(reply.Items, AuthoringItem{ID: "action:" + name, Kind: "action", Name: name, Description: action.Description, Edit: edit})
		}
		return
	}
	actionNode := mapValue(target.tool, "action")
	if dynamicName(actionNode) {
		reply.unavailable("unresolved-dynamic")
		return
	}
	actionName := stringValue(actionNode)
	action, ok := metadata.Actions[actionName]
	if !ok {
		reply.unavailable("incomplete-identity")
		return
	}
	reply.Site.Action = actionName
	if req.Operation == "required-arguments" {
		requiredAuthoringEdit(source, target, action, &reply)
		return
	}
	if target.site.Kind != "argument" || target.node == nil {
		reply.unavailable("unsupported-context")
		return
	}
	for name, arg := range action.Arguments {
		if mapValue(target.args, name) != nil {
			continue
		}
		edit, ok := source.nameEdit(target, name)
		if !ok {
			reply.unavailable("invalid-source-range")
			return
		}
		required := arg.Required
		defaultInfo := "absent"
		if arg.HasDefault {
			defaultInfo = "declared-redacted"
		}
		reply.Items = append(reply.Items, AuthoringItem{ID: "argument:" + name, Kind: "argument", Name: name, Description: arg.Description, ValueType: arg.Type, Required: &required, DefaultInfo: defaultInfo, Edit: edit})
	}
	return
}
func dynamicName(n *yaml.Node) bool {
	return n != nil && (strings.Contains(n.Value, "${") || strings.Contains(n.Value, "@{"))
}
func boundAuthoringMetadata(snapshot *bindingSnapshot, id string) (internaltool.AuthoringMetadata, bool) {
	def, ok := snapshot.definitions[id]
	if !ok {
		return internaltool.AuthoringMetadata{}, false
	}
	return definitionAuthoringMetadata(snapshot, def)
}
func definitionAuthoringMetadata(snapshot *bindingSnapshot, def runtimetool.ToolDef) (internaltool.AuthoringMetadata, bool) {
	var data []byte
	if def.SourcePath != "" {
		var err error
		data, err = snapshot.files.read(def.SourcePath)
		if err != nil {
			return internaltool.AuthoringMetadata{}, false
		}
	}
	metadata, err := internaltool.AuthoringProjection(data, def)
	return metadata, err == nil
}

func completeToolReference(ctx context.Context, req AuthoringRequest, s *authoringSource, t *authoringTarget, snapshot *bindingSnapshot, reply *AuthoringReply) {
	var ref schema.ToolRef
	if t.ref.Decode(&ref) != nil {
		reply.unavailable("incomplete-identity")
		return
	}
	for _, n := range []*yaml.Node{mapValue(t.ref, "name"), mapValue(t.ref, "package"), mapValue(t.ref, "version"), mapValue(t.ref, "path")} {
		if dynamicName(n) {
			reply.unavailable("unresolved-dynamic")
			return
		}
	}
	names := map[string]bool{}
	if ref.Path != "" {
		kind := pkgpath.KindToolRefsPath
		if req.Context.PackageRoot != "" {
			kind = pkgpath.KindToolRefsPathPackageInternal
		}
		path, _, err := pkgpath.ResolveKind(kind, req.Document.Path, ref.Path, req.Context.ProjectRoot, req.Context.PackageRoot)
		if err != nil {
			reply.unavailable("incomplete-identity")
			return
		}
		data, err := snapshot.files.read(path)
		if err != nil {
			reply.unavailable("missing-dependency")
			return
		}
		name, err := internaltool.AuthoringIdentity(data)
		if err != nil {
			reply.unavailable("incomplete-identity")
			return
		}
		names[name] = true
	} else {
		if snapshot.catalogReason != "" {
			reply.unavailable(reply.Discovery.Reason)
			return
		}
		for _, entry := range snapshot.catalog.Entries() {
			if entry.Bare != "" {
				names[entry.Bare] = true
			}
		}
	}
	if len(names) > MaxEntries {
		reply.unavailable("limit-exceeded")
		return
	}
	existing := map[string]bool{}
	refs := mapValue(s.root, "toolRefs")
	for _, n := range refs.Content {
		if n != t.ref {
			existing[stringValue(mapValue(n, "name"))] = true
		}
	}
	for name := range names {
		if ctx.Err() != nil {
			return
		}
		if existing[name] {
			continue
		}
		candidate := ref
		candidate.Name = name
		bound, errs := pkgcatalog.BindFile(snapshot.catalog, req.Document.Path, []*schema.ToolRef{&candidate}, req.Context.PackageRoot)
		fatal, _ := errkit.SplitWarnings(errs)
		if len(fatal) > 0 || len(bound) != 1 {
			continue
		}
		edit, ok := s.nameEdit(t, name)
		if !ok {
			reply.unavailable("invalid-source-range")
			return
		}
		item := AuthoringItem{ID: "reference:" + name, Kind: "tool", Name: name, Edit: edit}
		if metadata, ok := definitionAuthoringMetadata(snapshot, bound[0].Def); ok {
			item.Description = metadata.Description
		}
		reply.Items = append(reply.Items, item)
	}
}

func requiredAuthoringEdit(s *authoringSource, t *authoringTarget, action internaltool.AuthoringAction, reply *AuthoringReply) {
	args := t.args
	if args != nil && (args.Style&yaml.FlowStyle != 0 || args.Kind != yaml.MappingNode && !(args.Kind == yaml.ScalarNode && args.Tag == "!!null" && args.Value == "")) {
		reply.unavailable("unsupported-context")
		return
	}
	names := []string{}
	for name, arg := range action.Arguments {
		if arg.Required && mapValue(args, name) == nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return
	}
	container := args
	path := strings.TrimSuffix(strings.TrimSuffix(t.site.YAMLPath, "/name"), "/action")
	if args == nil {
		container = t.tool
	} else {
		path = strings.TrimSuffix(path, "/args") + "/args"
	}
	r, ok := s.mapping(container)
	if !ok {
		reply.unavailable("unsupported-context")
		return
	}
	insert, ok := authoringByteOffset(s.original, r.End)
	if !ok {
		reply.unavailable("invalid-source-range")
		return
	}
	newline := "\n"
	if strings.Contains(s.original, "\r\n") {
		newline = "\r\n"
	}
	indent := t.tool.Column + 1
	text := ""
	if args == nil {
		if insert > 0 && s.original[insert-1] != '\n' {
			text += newline
		}
		text += strings.Repeat(" ", t.tool.Column-1) + "args:" + newline
	} else if args.Kind == yaml.MappingNode {
		indent = args.Column - 1
		if insert > 0 && s.original[insert-1] != '\n' {
			text += newline
		}
	} else {
		// Empty args owns a zero-width scalar after its colon. Insert after
		// any trailing comment, keeping the original line byte-for-byte.
		if end := strings.IndexByte(s.original[insert:], '\n'); end >= 0 {
			insert += end + 1
		} else {
			text += newline
		}
		r.End = utf16Length(s.original[:insert])
	}
	out := AuthoringRequiredEdit{Placeholders: []Range{}}
	for _, name := range names {
		text += strings.Repeat(" ", indent) + yamlName(name) + ": "
		at := utf16Length(text)
		out.Placeholders = append(out.Placeholders, Range{Start: at, End: at + 4})
		text += "null" + newline
	}
	out.Edit = AuthoringEdit{Range: Range{Start: r.End, End: r.End}, NewText: text}
	reply.Site.Kind, reply.Site.YAMLPath, reply.Site.Range = "argument", path, r
	reply.RequiredEdit = &out
}

func resolveAuthoringExpression(ctx context.Context, s *authoringSource, t *authoringTarget, reply *AuthoringReply) {
	if t.mode == ExpressionRegex {
		return
	}
	if t.mode == ExpressionGIS && len(gis.ScanExpressions(ctx, t.node.Value)) == 0 {
		return
	}
	if utf16Length(t.node.Value) > 32768 {
		reply.Site = &t.site
		reply.unavailable("limit-exceeded")
		return
	}
	m, ok := s.decodeMap(t)
	if !ok {
		reply.Site = &t.site
		reply.unavailable("invalid-source-range")
		return
	}
	caret := -1
	for i, offset := range m.offsets {
		if offset == s.caret {
			caret = i
			break
		}
	}
	if caret < 0 {
		reply.Site = &t.site
		reply.unavailable("invalid-source-range")
		return
	}
	value := t.node.Value
	start, end := 0, len(value)
	if t.mode == ExpressionGIS || t.boolean && strings.HasPrefix(strings.TrimSpace(value), "${") {
		found := false
		for _, block := range gis.ScanExpressions(ctx, value) {
			if block.ExpressionStart <= caret && caret <= block.ExpressionEnd {
				start, end = block.ExpressionStart, block.ExpressionEnd
				found = true
				break
			}
		}
		if !found {
			return
		}
	}
	result := parser.Authoring(ctx, value[start:end], caret-start)
	if result.Reason != "" {
		reply.Site = &t.site
		reply.unavailable(result.Reason)
		return
	}
	if !result.Applicable && result.Function == nil {
		return
	}
	reply.Site = &t.site
	mapRange := func(a, b int, envelope bool) (Range, bool) {
		a, b, ok := m.mapped(start+a, start+b, envelope)
		if !ok {
			return Range{}, false
		}
		return Range{Start: utf16Length(s.original[:a]), End: utf16Length(s.original[:b])}, true
	}
	if reply.Operation == "complete" && result.Applicable {
		r, ok := mapRange(result.Start, result.End, false)
		if !ok {
			reply.unavailable("invalid-source-range")
			return
		}
		for _, item := range result.Items {
			text := m.encode(item.Text)
			if t.node.Value == "" {
				if s.caret > 0 && s.original[s.caret-1] == ':' {
					text = " " + text
				}
				if s.caret < len(s.original) && s.original[s.caret] == '#' {
					text += " "
				}
			}
			reply.Items = append(reply.Items, AuthoringItem{ID: item.Kind + ":" + item.Name, Kind: item.Kind, Name: item.Name, Edit: AuthoringEdit{Range: r, NewText: text}})
		}
		if s.typed {
			completeTypedBindings(s, t, reply, value[start:end], result.Start, caret-start, r, m.encode)
		}
	}
	if reply.Operation == "signature" && result.Function != nil {
		r, ok := mapRange(result.CallStart, result.CallEnd, true)
		if !ok {
			reply.unavailable("invalid-source-range")
			return
		}
		d := result.Function
		signature := AuthoringSignature{Name: d.Name, Label: d.Label, Parameters: []AuthoringParameter{}, CallRange: r}
		offset := strings.IndexByte(d.Label, '(') + 1
		for _, parameter := range d.Parameters() {
			name := strings.SplitN(parameter, ":", 2)[0]
			signature.Parameters = append(signature.Parameters, AuthoringParameter{Name: name, LabelRange: Range{Start: offset, End: offset + len(parameter)}})
			offset += len(parameter) + 2
		}
		if result.Active < len(signature.Parameters) {
			active := result.Active
			signature.ActiveParameter = &active
		}
		reply.Signature = &signature
	}
}
