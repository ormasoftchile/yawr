package graphdoc

import (
	"context"
	"fmt"
	"sort"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// Builder produces a Document by walking a runbook AST via flowwalk.
//
// Recurse controls how include steps are rendered:
//   - false (default): include nodes are opaque leaves; the included
//     runbook's structure is not added to the Document. Useful for
//     navigation views where each include is its own collapsible card.
//   - true: included runbooks are inlined as nested Frames whose top-level
//     nodes hang under an include-frame Group attached to the include node.
//
// RecurseIncludeIDs, when non-nil, takes precedence over Recurse and
// selectively inlines only the listed include step IDs. This lets
// callers (notably the run-document handler) inline just the includes
// the runtime has actually entered, while leaving every other include
// opaque. Includes not in the set are also not loaded from disk —
// the visitor's BeforeInclude hook tells the walker to skip them.
//
// Loader is required when Recurse=true or RecurseIncludeIDs is non-empty.
// When neither is set, includes are not opened, so Loader may be nil.
type Builder struct {
	Loader            flowwalk.Loader
	Recurse           bool
	RecurseIncludeIDs map[string]bool
	MaxDepth          int
}

// Build walks rb and returns the populated Document. The Document's Hash
// field is set to a content-hash over the rest of the document.
func (b *Builder) Build(ctx context.Context, rb *parser.ParsedRunbook) (*Document, error) {
	if rb == nil || rb.Runbook == nil {
		return nil, fmt.Errorf("graphdoc: nil runbook")
	}

	v := newDocVisitor(b.Recurse, b.RecurseIncludeIDs)
	v.doc.Runbook = RunbookRef{
		ID:   rb.Runbook.ID,
		Name: rb.Runbook.Name,
		Path: rb.Source,
	}
	v.doc.Regions = rb.Runbook.Regions
	v.doc.Inputs = buildInputDecls(rb.Runbook)

	w := &flowwalk.Walker{
		Loader:   b.Loader,
		MaxDepth: b.MaxDepth,
	}

	if err := w.Walk(ctx, rb, v); err != nil {
		return nil, err
	}
	v.finalizeRuntimeIDs()

	v.doc.SchemaVersion = SchemaVersion
	for _, frame := range v.doc.Frames {
		if frame.Invocation != nil {
			v.doc.SchemaVersion = "3"
		}
	}
	for _, node := range v.doc.Nodes {
		if node.Kind == "assign" || node.Kind == "results" {
			v.doc.SchemaVersion = "3"
		}
	}
	hash, err := v.doc.computeHash()
	if err != nil {
		return nil, err
	}
	v.doc.Hash = hash
	return v.doc, nil
}

func (v *docVisitor) finalizeRuntimeIDs() {
	nodeRaw := make(map[string]string, len(v.doc.Nodes))
	for i := range v.doc.Nodes {
		node := &v.doc.Nodes[i]
		qualifiedID := node.ID
		rawID := node.StepID
		if rawID == "" {
			rawID = qualifiedID
		}
		nodeRaw[qualifiedID] = rawID
		node.ID = rawID
		if qualifiedID != rawID {
			node.QualifiedID = qualifiedID
		}
	}

	frameRaw := make(map[string]string, len(v.doc.Frames))
	for i := range v.doc.Frames {
		frame := &v.doc.Frames[i]
		qualifiedID := frame.ID
		rawID := qualifiedID
		if frame.ParentIncludeNodeID != "" {
			rawID = "frame:" + nodeRaw[frame.ParentIncludeNodeID]
			frame.QualifiedParentIncludeNodeID = frame.ParentIncludeNodeID
			frame.ParentIncludeNodeID = nodeRaw[frame.ParentIncludeNodeID]
		}
		frameRaw[qualifiedID] = rawID
		frame.ID = rawID
		if qualifiedID != rawID {
			frame.QualifiedID = qualifiedID
		}
	}

	groupRaw := make(map[string]string, len(v.doc.Groups))
	for i := range v.doc.Groups {
		group := &v.doc.Groups[i]
		qualifiedID := group.ID
		qualifiedParentID := group.ParentNodeID
		qualifiedFrameID := group.FrameID
		rawParentID := nodeRaw[qualifiedParentID]
		rawID := fmt.Sprintf("group:%s:%s:%d", rawParentID, group.Kind, group.Index)
		groupRaw[qualifiedID] = rawID
		group.ID = rawID
		group.ParentNodeID = rawParentID
		group.FrameID = frameRaw[qualifiedFrameID]
		if qualifiedID != rawID {
			group.QualifiedID = qualifiedID
		}
		if qualifiedParentID != rawParentID {
			group.QualifiedParentNodeID = qualifiedParentID
		}
		if qualifiedFrameID != "" && qualifiedFrameID != group.FrameID {
			group.QualifiedFrameID = qualifiedFrameID
		}
	}

	for i := range v.doc.Nodes {
		node := &v.doc.Nodes[i]
		qualifiedFrameID := node.FrameID
		qualifiedGroupID := node.GroupID
		node.FrameID = frameRaw[qualifiedFrameID]
		node.GroupID = groupRaw[qualifiedGroupID]
		if qualifiedFrameID != node.FrameID {
			node.QualifiedFrameID = qualifiedFrameID
		}
		if qualifiedGroupID != "" && qualifiedGroupID != node.GroupID {
			node.QualifiedGroupID = qualifiedGroupID
		}
	}

	for i := range v.doc.Edges {
		edge := &v.doc.Edges[i]
		qualifiedFrom, qualifiedTo := edge.From, edge.To
		edge.From, edge.To = nodeRaw[qualifiedFrom], nodeRaw[qualifiedTo]
		if qualifiedFrom != edge.From {
			edge.QualifiedFrom = qualifiedFrom
		}
		if qualifiedTo != edge.To {
			edge.QualifiedTo = qualifiedTo
		}
	}
}

// buildInputDecls builds the root runbook's input-declaration DTO list
// (AR-CE-2, T-CLIENT-ENUM-DTO): one InputDecl per `inputs.<name>` key,
// name-sorted for a stable, content-hashable Document. Declared enum
// member ORDER within each declaration is preserved verbatim (AR-ENUM-5);
// only the top-level name-to-name ordering is imposed here for hash
// determinism (Go map iteration order is not defined). Redaction is
// computed the same way plan-time EnumMeta.Redacted is (C1), via the
// shared schema.IsRedactedDeclName helper, so a preview document built
// before planning and a live run's plan.validated payload never disagree
// about which declarations are redacted.
func buildInputDecls(rb *schema.Runbook) []InputDecl {
	if rb == nil || len(rb.Inputs) == 0 {
		return nil
	}
	names := make([]string, 0, len(rb.Inputs))
	for name := range rb.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]InputDecl, 0, len(names))
	for _, name := range names {
		in := rb.Inputs[name]
		if in == nil {
			continue
		}
		typ := in.Type
		if typ == "" {
			typ = "string"
		}
		decl := InputDecl{
			Name:        name,
			Type:        typ,
			Required:    in.Required,
			Description: in.Description,
		}
		if len(in.Enum) > 0 {
			if schema.IsRedactedDeclName("inputs."+name, rb.Governance) {
				decl.EnumRedacted = true
				decl.EnumMemberCount = len(in.Enum)
				// Default is intentionally omitted too when redacted
				// (AR-CE-2 §2 table: "omitted when redacted").
			} else {
				decl.Enum = append([]string(nil), in.Enum...)
				decl.Default = in.Default
			}
		} else {
			decl.Default = in.Default
		}
		out = append(out, decl)
	}
	return out
}

// scope is one level on the visitor's container stack. It records the
// current frame, an optional enclosing group, the entry edge to attach to
// the first node emitted in this scope, and the previously-emitted sibling
// (for sequence edges).
type scope struct {
	frameID   string
	groupID   string
	entryEdge *pendingEdge
	last      string // last sibling node ID in this scope, "" if none
}

type pendingEdge struct {
	from  string
	kind  EdgeKind
	label string
}

// docVisitor builds a Document while traversing the AST.
type docVisitor struct {
	flowwalk.Base

	recurse    bool
	recurseIDs map[string]bool // when non-nil, only these include IDs recurse
	doc        *Document

	// stack is the active container stack (root frame first).
	stack []*scope

	// orderSeq is the pre-order Node.Order counter.
	orderSeq int
	callPath []enginepkg.DebugCallFrame

	// includeNodeStack tracks the include node ID for each nested include
	// frame, so EnterInclude can record its frame's ParentIncludeNodeID.
	includeNodeStack []string
	includeExpanded  []bool

	// frameDepth is the current include depth (root = 0).
	frameDepth int
}

func newDocVisitor(recurse bool, recurseIDs map[string]bool) *docVisitor {
	return &docVisitor{
		recurse:    recurse,
		recurseIDs: recurseIDs,
		doc: &Document{
			Frames: []Frame{},
			Groups: []Group{},
			Nodes:  []Node{},
			Edges:  []Edge{},
		},
	}
}

// shouldRecurse reports whether the include with the given step ID
// should be expanded into a nested frame. When recurseIDs is non-nil it
// takes precedence over the global recurse flag.
func (v *docVisitor) shouldRecurse(stepID string) bool {
	if v.recurseIDs != nil {
		return v.recurseIDs[enginepkg.DebugNodeID(v.callPath, stepID)] || v.recurseIDs[stepID]
	}
	return v.recurse
}

// BeforeInclude tells the walker whether to load the included runbook.
// We skip loading entirely for opaque includes so the server doesn't
// pay disk + parse cost for sub-runbooks the renderer is not going to
// inline.
//
// Dynamic include sites (runbook_ref) are always skipped regardless of
// the Recurse setting — their target is unknown at preview time.
func (v *docVisitor) BeforeInclude(_ flowwalk.Ctx, s *schema.Step) (bool, error) {
	if s.IncludeSpec != nil && s.IncludeSpec.Include.IsDynamic() {
		return false, nil
	}
	return v.shouldRecurse(s.ID), nil
}

// EnterRunbook pushes the root frame onto the stack.
func (v *docVisitor) EnterRunbook(ctx flowwalk.Ctx, rb *parser.ParsedRunbook) error {
	contentHash, err := runbookContentHash(rb.Runbook)
	if err != nil {
		return err
	}
	frame := Frame{
		ID:          "frame:root",
		Invocation:  typedInvocationDetails(rb.Runbook),
		RunbookID:   rb.Runbook.ID,
		RunbookPath: rb.Source,
		ContentHash: contentHash,
		Depth:       0,
	}
	v.doc.Frames = append(v.doc.Frames, frame)
	v.push(&scope{frameID: frame.ID})
	return nil
}

// LeaveRunbook pops the root frame.
func (v *docVisitor) LeaveRunbook(_ flowwalk.Ctx, _ *parser.ParsedRunbook) error {
	v.pop()
	return nil
}

// EnterStep emits a Node for every step kind except include (which is
// handled in EnterInclude so its frame/group plumbing happens before
// children traverse).
func (v *docVisitor) EnterStep(_ flowwalk.Ctx, s *schema.Step) error {
	if s.Type == schema.StepTypeInclude {
		return nil
	}
	v.emitNode(s.ID, string(s.Type), displayTitle(s))
	node := &v.doc.Nodes[len(v.doc.Nodes)-1]
	node.Details = detailsForStep(s)
	if s.Type == schema.StepTypeTool && s.ToolCall != nil {
		node.ToolName = s.ToolCall.Tool.Name
		node.ToolAction = s.ToolCall.Tool.Action
	}
	return nil
}

// EnterIterate emits the iterate node and pushes its body group.
func (v *docVisitor) EnterIterate(_ flowwalk.Ctx, iter *schema.IterateNode) error {
	nodeID := v.emitNode(iter.ID, string(schema.StepTypeIterate), iterateLabel(iter))
	v.doc.Nodes[len(v.doc.Nodes)-1].Details = detailsForIterate(iter)
	if iter.Concurrency > 1 {
		v.doc.Nodes[len(v.doc.Nodes)-1].Concurrent = true
	}
	v.pushGroup(GroupIterateBody, nodeID, iterateLabel(iter), 0, &pendingEdge{
		from: nodeID, kind: EdgeIterateBody,
	})
	v.pushCall(iter.ID)
	return nil
}

func (v *docVisitor) LeaveIterate(_ flowwalk.Ctx, _ *schema.IterateNode) error {
	v.popCall()
	v.pop()
	return nil
}

// EnterParallel emits the parallel node; per-branch groups are pushed in
// EnterParallelBranch.
func (v *docVisitor) EnterParallel(_ flowwalk.Ctx, par *schema.ParallelNode) error {
	v.emitNode(par.ID, string(schema.StepTypeParallel), "")
	v.doc.Nodes[len(v.doc.Nodes)-1].Details = detailsForParallel(par)
	return nil
}

func (v *docVisitor) EnterParallelBranch(_ flowwalk.Ctx, par *schema.ParallelNode, idx int, br *schema.ParallelBranch) error {
	label := br.Label
	parentID := enginepkg.DebugNodeID(v.callPath, par.ID)
	v.pushGroup(GroupParallelBranch, parentID, label, idx, &pendingEdge{
		from: parentID, kind: EdgeParallelBranch, label: label,
	})
	return nil
}

func (v *docVisitor) LeaveParallelBranch(_ flowwalk.Ctx, _ *schema.ParallelNode, _ int, _ *schema.ParallelBranch) error {
	v.pop()
	return nil
}

// EnterArm pushes a branch-arm group. The branch parent node was already
// emitted by EnterStep.
func (v *docVisitor) EnterArm(_ flowwalk.Ctx, parent *schema.Step, idx int, arm *schema.BranchArm) error {
	label := branchArmLabel(arm)
	edgeLabel := label
	if arm.Else {
		edgeLabel = "Otherwise"
		if label != "" {
			edgeLabel += " — " + label
		}
	}
	parentID := enginepkg.DebugNodeID(v.callPath, parent.ID)
	v.pushGroup(GroupBranchArm, parentID, label, idx, &pendingEdge{
		from: parentID, kind: EdgeBranchArm, label: edgeLabel,
	})
	v.doc.Groups[len(v.doc.Groups)-1].Fallback = arm.Else
	v.pushCall(parent.ID)
	return nil
}

func (v *docVisitor) LeaveArm(_ flowwalk.Ctx, _ *schema.Step, _ int, _ *schema.BranchArm) error {
	v.popCall()
	v.pop()
	return nil
}

// EnterCompensate pushes a compensate-body group attached to the parent
// step that owns the compensate.
func (v *docVisitor) EnterCompensate(_ flowwalk.Ctx, parent *schema.Step) error {
	parentID := enginepkg.DebugNodeID(v.callPath, parent.ID)
	v.pushGroup(GroupCompensateBody, parentID, "", 0, &pendingEdge{
		from: parentID, kind: EdgeCompensate,
	})
	return nil
}

func (v *docVisitor) LeaveCompensate(_ flowwalk.Ctx, _ *schema.Step) error {
	v.pop()
	return nil
}

// EnterInclude emits the include node. With Recurse=true, it then opens a
// new frame and pushes an include-frame group so the included runbook's
// top-level nodes hang under the include node.
//
// Dynamic include sites (runbook_ref + resolve_from: catalog) are always
// emitted as leaf nodes with Dynamic=true and the ⟨dynamic⟩ title prefix
// regardless of the Recurse setting — their target is unknown at preview
// time so there is never a child frame to inline.
func (v *docVisitor) EnterInclude(_ flowwalk.Ctx, s *schema.Step, child *parser.ParsedRunbook) (bool, error) {
	v.includeExpanded = append(v.includeExpanded, false)
	isDynamic := s.IncludeSpec != nil && s.IncludeSpec.Include.IsDynamic()
	title := displayTitle(s)
	if isDynamic {
		title = fmt.Sprintf("⟨dynamic⟩ %s", s.IncludeSpec.Include.RunbookRef)
	}
	includeNodeID := v.emitNode(s.ID, string(schema.StepTypeInclude), title)
	v.doc.Nodes[len(v.doc.Nodes)-1].Details = detailsForStep(s)
	if child != nil && child.Runbook != nil && v.doc.Nodes[len(v.doc.Nodes)-1].Details != nil {
		v.doc.Nodes[len(v.doc.Nodes)-1].Details.Steps = len(child.Runbook.Flow)
	}
	if isDynamic {
		v.doc.Nodes[len(v.doc.Nodes)-1].Dynamic = true
		return false, nil
	}

	if !v.shouldRecurse(s.ID) {
		return false, nil
	}
	if child == nil {
		// Loader was missing or returned nil; treat as opaque.
		return false, nil
	}
	contentHash, err := runbookContentHash(child.Runbook)
	if err != nil {
		return false, err
	}

	v.frameDepth++
	frame := Frame{
		ID:                  fmt.Sprintf("frame:%s", includeNodeID),
		Invocation:          typedInvocationDetails(child.Runbook),
		RunbookID:           child.Runbook.ID,
		RunbookPath:         child.Source,
		ContentHash:         contentHash,
		ParentIncludeNodeID: includeNodeID,
		Depth:               v.frameDepth,
	}
	v.doc.Frames = append(v.doc.Frames, frame)
	v.includeNodeStack = append(v.includeNodeStack, includeNodeID)

	// Push a fresh top-level scope inside the new frame, with the
	// include-frame group as its container. Sequence edges between top-
	// level nodes of the included runbook flow through this group.
	groupID := fmt.Sprintf("group:%s:%s:0", includeNodeID, GroupIncludeFrame)
	v.doc.Groups = append(v.doc.Groups, Group{
		ID:           groupID,
		Kind:         GroupIncludeFrame,
		ParentNodeID: includeNodeID,
		FrameID:      frame.ID,
	})
	v.stack = append(v.stack, &scope{
		frameID: frame.ID,
		groupID: groupID,
		entryEdge: &pendingEdge{
			from: includeNodeID, kind: EdgeInclude,
		},
	})
	v.pushCall(s.ID)
	v.includeExpanded[len(v.includeExpanded)-1] = true
	return true, nil
}

func (v *docVisitor) LeaveInclude(_ flowwalk.Ctx, s *schema.Step, child *parser.ParsedRunbook) error {
	if len(v.includeExpanded) == 0 {
		return nil
	}
	expanded := v.includeExpanded[len(v.includeExpanded)-1]
	v.includeExpanded = v.includeExpanded[:len(v.includeExpanded)-1]
	if !expanded {
		return nil
	}
	v.popCall()
	v.pop()
	v.includeNodeStack = v.includeNodeStack[:len(v.includeNodeStack)-1]
	v.frameDepth--
	return nil
}

// emitNode appends a Node to the current scope, consumes any pending entry
// edge, and otherwise emits a sequence edge from the previous sibling.
func (v *docVisitor) emitNode(stepID, kind, title string) string {
	top := v.top()
	id := enginepkg.DebugNodeID(v.callPath, stepID)
	callPath := make([]string, len(v.callPath))
	for i, frame := range v.callPath {
		callPath[i] = frame.StepID
	}
	n := Node{
		ID:       id,
		StepID:   stepID,
		CallPath: callPath,
		Kind:     kind,
		Title:    title,
		FrameID:  top.frameID,
		GroupID:  top.groupID,
		Order:    v.orderSeq,
	}
	v.orderSeq++
	v.doc.Nodes = append(v.doc.Nodes, n)

	switch {
	case top.entryEdge != nil:
		v.doc.Edges = append(v.doc.Edges, Edge{
			From:  top.entryEdge.from,
			To:    id,
			Kind:  top.entryEdge.kind,
			Label: top.entryEdge.label,
		})
		top.entryEdge = nil
	case top.last != "":
		v.doc.Edges = append(v.doc.Edges, Edge{
			From: top.last,
			To:   id,
			Kind: EdgeSequence,
		})
	}
	top.last = id
	return id
}

// pushGroup creates a Group and pushes a matching scope onto the stack.
func (v *docVisitor) pushGroup(kind GroupKind, parentNodeID, label string, index int, entry *pendingEdge) {
	frameID := v.top().frameID
	id := fmt.Sprintf("group:%s:%s:%d", parentNodeID, kind, index)
	v.doc.Groups = append(v.doc.Groups, Group{
		ID:           id,
		Kind:         kind,
		ParentNodeID: parentNodeID,
		FrameID:      frameID,
		Label:        label,
		Index:        index,
	})
	v.push(&scope{
		frameID:   frameID,
		groupID:   id,
		entryEdge: entry,
	})
}

func (v *docVisitor) push(s *scope) { v.stack = append(v.stack, s) }
func (v *docVisitor) pop()          { v.stack = v.stack[:len(v.stack)-1] }
func (v *docVisitor) top() *scope   { return v.stack[len(v.stack)-1] }
func (v *docVisitor) pushCall(stepID string) {
	v.callPath = append(v.callPath, enginepkg.DebugCallFrame{StepID: stepID})
}
func (v *docVisitor) popCall() { v.callPath = v.callPath[:len(v.callPath)-1] }

// displayTitle returns the step's human-readable title, falling back to ID.
func displayTitle(s *schema.Step) string {
	if s.Title != "" {
		return s.Title
	}
	if s.Type == schema.StepTypeResults {
		return "Results"
	}
	return s.ID
}

// iterateLabel returns a short label for an iterate node
// ("over <expr> as <var>"), falling back to the node's ID.
func iterateLabel(iter *schema.IterateNode) string {
	switch {
	case iter.Over != "" && iter.As != "":
		return "over " + iter.Over + " as " + iter.As
	case iter.Over != "":
		return "over " + iter.Over
	}
	return iter.ID
}

// branchArmLabel returns an arm's descriptive label. Fallback identity is
// carried separately on Group.Fallback so renderers can say "Otherwise".
func branchArmLabel(arm *schema.BranchArm) string {
	if arm.Label != "" {
		return arm.Label
	}
	if arm.Condition != "" {
		return arm.Condition
	}
	return ""
}
