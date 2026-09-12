package serve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/flowwalk"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

const maxDebugProfileBytes = 64 * 1024

var debugProfileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var debugProfileRename = os.Rename

var (
	errDebugProfileOutsideWorkspace = errors.New("debug profile runbook must be inside the workspace root")
	errDebugProfileSymlinkPath      = errors.New("debug profile runbook path must not traverse a symlink or junction")
)

type DebugProfileListItem struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Stale   bool         `json:"stale"`
	Profile DebugProfile `json:"profile"`
}

type debugProfileSaveRequest struct {
	RunbookPath string       `json:"runbookPath"`
	Profile     DebugProfile `json:"profile"`
}

type debugProfileMetadata struct {
	rootRef        string
	rootID         string
	planHash       string
	protectedVars  map[string]bool
	protectAllVars bool
}

func (s *Server) handleDebugProfilesList(w http.ResponseWriter, r *http.Request) {
	runbookPath := strings.TrimSpace(r.URL.Query().Get("runbookPath"))
	if runbookPath == "" {
		http.Error(w, "runbookPath is required", http.StatusBadRequest)
		return
	}
	metadata, err := s.debugProfileMetadata(r, runbookPath)
	if err != nil {
		if errors.Is(err, errDebugProfileOutsideWorkspace) || errors.Is(err, errDebugProfileSymlinkPath) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{"profiles": []DebugProfileListItem{}})
			return
		}
		writeRunbookError(w, err)
		return
	}

	s.debugProfilesMu.Lock()
	profiles, err := s.readDebugProfiles(metadata)
	s.debugProfilesMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"profiles": profiles})
}

func (s *Server) handleDebugProfilePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !debugProfileIDPattern.MatchString(id) {
		http.Error(w, "invalid debug profile id", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDebugProfileBytes+1))
	if err != nil || len(body) > maxDebugProfileBytes {
		http.Error(w, "invalid debug profile body", http.StatusBadRequest)
		return
	}
	var request debugProfileSaveRequest
	if err := decodeStrictJSON(body, &request); err != nil || strings.TrimSpace(request.RunbookPath) == "" {
		http.Error(w, "invalid debug profile JSON", http.StatusBadRequest)
		return
	}
	metadata, err := s.debugProfileMetadata(r, request.RunbookPath)
	if err != nil {
		writeRunbookError(w, err)
		return
	}

	profile := request.Profile
	profile.Version = debugProfileVersion
	profile.Root = DebugProfileRoot{Ref: metadata.rootRef, ID: metadata.rootID}
	profile.CreatedAgainst = metadata.planHash
	if err := validateDebugProfile(&profile); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateDebugProfileProtectedVars(&profile, metadata.protectedVars, metadata.protectAllVars); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	encoded, err := yaml.Marshal(profile)
	if err != nil || len(encoded) > maxDebugProfileBytes {
		http.Error(w, "debug profile is too large", http.StatusBadRequest)
		return
	}

	s.debugProfilesMu.Lock()
	err = s.writeDebugProfile(id, encoded)
	s.debugProfilesMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(DebugProfileListItem{ID: id, Name: profile.Name, Profile: profile})
}

func (s *Server) debugProfileMetadata(r *http.Request, runbookPath string) (debugProfileMetadata, error) {
	canonicalRunbook, rootRef, err := s.canonicalDebugRunbookPath(runbookPath)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	plan, _, _, _, err := s.loadPlanAndSeed(r.Context(), canonicalRunbook, nil)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	closureHash, closureProtectedVars, protectAllVars, err := s.debugRunbookClosureMetadata(r.Context(), canonicalRunbook)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	planHash, err := debugPlanHash(plan, closureHash)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	return debugProfileMetadata{
		rootRef: rootRef, rootID: plan.Metadata.RunbookID, planHash: planHash,
		protectedVars:  mergeDebugProtectedVars(debugProtectedVars(plan), closureProtectedVars),
		protectAllVars: protectAllVars,
	}, nil
}

func (s *Server) debugProfileMetadataForPlan(ctx context.Context, runbookPath string, plan *engine.ExecutionPlan) (debugProfileMetadata, error) {
	canonicalRunbook, rootRef, err := s.canonicalDebugRunbookPath(runbookPath)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	closureHash, closureProtectedVars, protectAllVars, err := s.debugRunbookClosureMetadata(ctx, canonicalRunbook)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	planHash, err := debugPlanHash(plan, closureHash)
	if err != nil {
		return debugProfileMetadata{}, err
	}
	return debugProfileMetadata{
		rootRef: rootRef, rootID: plan.Metadata.RunbookID, planHash: planHash,
		protectedVars:  mergeDebugProtectedVars(debugProtectedVars(plan), closureProtectedVars),
		protectAllVars: protectAllVars,
	}, nil
}

func validateDebugProfileBinding(profile *DebugProfile, metadata debugProfileMetadata, allowStale bool) error {
	if profile == nil {
		return nil
	}
	if profile.Root.Ref != metadata.rootRef || profile.Root.ID != metadata.rootID {
		return errors.New("debug profile is bound to a different root runbook")
	}
	if profile.CreatedAgainst != "" && metadata.planHash != "" && profile.CreatedAgainst != metadata.planHash && !allowStale {
		return errors.New("debug profile is stale; explicit confirmation is required")
	}
	if err := validateDebugProfileProtectedVars(profile, metadata.protectedVars, metadata.protectAllVars); err != nil {
		return err
	}
	return nil
}

func validateDebugProfileProtectedVars(profile *DebugProfile, protected map[string]bool, protectAll bool) error {
	if profile == nil {
		return nil
	}
	for _, override := range profile.Overrides {
		for name := range override.Set.Vars {
			if protectAll {
				return errors.New("debug profile cannot persist variable overrides for a dynamically protected runbook")
			}
			if protected[name] {
				return fmt.Errorf("debug profile cannot override protected variable %q", name)
			}
		}
	}
	return nil
}

func debugProtectedVars(plan *engine.ExecutionPlan) map[string]bool {
	protected := make(map[string]bool)
	if plan == nil {
		return protected
	}
	for name, declaration := range plan.Inputs {
		if declaration != nil && declaration.Type == "secret" {
			protected[name] = true
		}
	}
	return protected
}

func mergeDebugProtectedVars(destination map[string]bool, source map[string]bool) map[string]bool {
	if destination == nil {
		destination = make(map[string]bool, len(source))
	}
	for name := range source {
		destination[name] = true
	}
	return destination
}

func (s *Server) debugProfileWorkspaceRoot() (string, error) {
	root := s.cfg.WorkspaceRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(root)
}

func (s *Server) canonicalDebugRunbookPath(runbookPath string) (string, string, error) {
	root, err := s.debugProfileWorkspaceRoot()
	if err != nil {
		return "", "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	target := runbookPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || !debugPathIsContained(rel) {
		return "", "", errDebugProfileOutsideWorkspace
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", err
	}
	expectedTarget := filepath.Join(canonicalRoot, rel)
	if !sameDebugPath(canonicalTarget, expectedTarget) {
		return "", "", errDebugProfileSymlinkPath
	}
	canonicalRel, err := filepath.Rel(canonicalRoot, canonicalTarget)
	if err != nil || !debugPathIsContained(canonicalRel) {
		return "", "", errDebugProfileOutsideWorkspace
	}
	return canonicalTarget, filepath.ToSlash(canonicalRel), nil
}

func debugPathIsContained(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func sameDebugPath(left, right string) bool {
	return sameDebugPathForOS(runtime.GOOS, left, right)
}

func sameDebugPathForOS(goos, left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if goos == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (s *Server) debugProfileDir(create bool) (string, error) {
	return s.debugProfileProductDir(".yawr", create)
}

func (s *Server) debugProfileProductDir(productDir string, create bool) (string, error) {
	root, err := s.debugProfileWorkspaceRoot()
	if err != nil {
		return "", err
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := root
	canonicalCurrent := canonicalRoot
	parts := []string{productDir, "debug-profiles"}
	for index, part := range parts {
		current = filepath.Join(current, part)
		expected := filepath.Join(canonicalCurrent, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if !create {
				return filepath.Join(append([]string{current}, parts[index+1:]...)...), nil
			}
			if err := os.Mkdir(current, 0o755); err != nil {
				return "", err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("debug profile storage path must contain only real directories")
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", err
		}
		if !sameDebugPath(resolved, expected) {
			return "", errors.New("debug profile storage path must not traverse a symlink or junction")
		}
		canonicalCurrent = resolved
	}
	return current, nil
}

func (s *Server) readDebugProfiles(metadata debugProfileMetadata) ([]DebugProfileListItem, error) {
	canonicalDir, err := s.debugProfileDir(false)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	entries, readErr := os.ReadDir(canonicalDir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 && filepath.Ext(entry.Name()) == ".yaml" {
			names[entry.Name()] = struct{}{}
		}
	}
	profiles := make([]DebugProfileListItem, 0)
	for name := range names {
		if len(profiles) >= 256 {
			continue
		}
		id := strings.TrimSuffix(name, ".yaml")
		if !debugProfileIDPattern.MatchString(id) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(canonicalDir, name))
		if readErr != nil {
			return nil, readErr
		}
		if len(data) > maxDebugProfileBytes {
			continue
		}
		var profile DebugProfile
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if decoder.Decode(&profile) != nil || validateDebugProfile(&profile) != nil {
			continue
		}
		if profile.Root.Ref != metadata.rootRef || profile.Root.ID != metadata.rootID {
			continue
		}
		profiles = append(profiles, DebugProfileListItem{
			ID:      id,
			Name:    profile.Name,
			Stale:   profile.CreatedAgainst != "" && metadata.planHash != "" && profile.CreatedAgainst != metadata.planHash,
			Profile: profile,
		})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func (s *Server) writeDebugProfile(id string, data []byte) error {
	dir, err := s.debugProfileDir(true)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".debug-profile-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return err
	}
	finalPath := filepath.Join(dir, id+".yaml")
	backupPath := finalPath + ".backup"
	if err := recoverDebugProfileBackup(finalPath, backupPath); err != nil {
		return err
	}
	if err := debugProfileRename(tempPath, finalPath); err == nil {
		return nil
	} else if _, statErr := os.Lstat(finalPath); statErr != nil {
		return err
	}

	if err := debugProfileRename(finalPath, backupPath); err != nil {
		return fmt.Errorf("backup previous debug profile: %w", err)
	}
	if err := debugProfileRename(tempPath, finalPath); err != nil {
		if restoreErr := debugProfileRename(backupPath, finalPath); restoreErr != nil {
			return errors.Join(
				fmt.Errorf("replace debug profile: %w", err),
				fmt.Errorf("restore previous debug profile: %w", restoreErr),
			)
		}
		return fmt.Errorf("replace debug profile: %w", err)
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove debug profile backup: %w", err)
	}
	return nil
}

func recoverDebugProfileBackup(finalPath, backupPath string) error {
	if _, err := os.Lstat(backupPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("remove stale debug profile backup: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := debugProfileRename(backupPath, finalPath); err != nil {
		return fmt.Errorf("recover previous debug profile: %w", err)
	}
	return nil
}

func debugPlanHash(plan *engine.ExecutionPlan, closureHash ...string) (string, error) {
	if plan == nil {
		return "", errors.New("debug profile plan is required")
	}
	steps := append([]engine.ResolvedStep(nil), plan.Steps...)
	for index := range steps {
		steps[index].Origin = ""
	}
	rootHash := plan.Metadata.PlanHash
	if plan.Validation != nil && plan.Validation.RunbookHash != "" {
		rootHash = plan.Validation.RunbookHash
	}
	payload := struct {
		RunbookID     string                     `json:"runbook_id"`
		RootHash      string                     `json:"root_hash"`
		CatalogDigest string                     `json:"catalog_digest,omitempty"`
		Steps         []engine.ResolvedStep      `json:"steps"`
		Tools         map[string]*schema.ToolDef `json:"tools,omitempty"`
		Inputs        map[string]*schema.Input   `json:"inputs,omitempty"`
		Outputs       map[string]*schema.Output  `json:"outputs,omitempty"`
		ClosureHash   string                     `json:"closure_hash,omitempty"`
	}{
		RunbookID: plan.Metadata.RunbookID, RootHash: rootHash,
		CatalogDigest: plan.Metadata.CatalogDigest, Steps: steps, Tools: plan.Tools,
		Inputs: plan.Inputs, Outputs: plan.Outputs,
	}
	if len(closureHash) > 0 {
		payload.ClosureHash = closureHash[0]
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serialize expanded debug profile plan: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:]), nil
}

type debugRunbookHashVisitor struct {
	flowwalk.Base
	entries        [][]byte
	protectedVars  map[string]bool
	protectAllVars bool
}

func (visitor *debugRunbookHashVisitor) EnterRunbook(_ flowwalk.Ctx, runbook *parserpkg.ParsedRunbook) error {
	return visitor.appendRunbook(runbook)
}

func (visitor *debugRunbookHashVisitor) appendRunbook(runbook *parserpkg.ParsedRunbook) error {
	if runbook == nil || runbook.Runbook == nil {
		return errors.New("debug profile runbook closure contains a nil runbook")
	}
	semantic, err := os.ReadFile(runbook.Source)
	if err != nil {
		return fmt.Errorf("read debug profile runbook closure %q: %w", runbook.Source, err)
	}
	visitor.entries = append(visitor.entries, semantic)
	if visitor.protectedVars == nil {
		visitor.protectedVars = make(map[string]bool)
	}
	for name, declaration := range runbook.Runbook.Inputs {
		if declaration != nil && declaration.Type == "secret" {
			visitor.protectedVars[name] = true
		}
	}
	return nil
}

func (visitor *debugRunbookHashVisitor) BeforeInclude(_ flowwalk.Ctx, step *schema.Step) (bool, error) {
	if step == nil || step.IncludeSpec == nil {
		return false, nil
	}
	if step.IncludeSpec.Include.IsDynamic() {
		visitor.protectAllVars = true
		return false, nil
	}
	return true, nil
}

func (visitor *debugRunbookHashVisitor) EnterInclude(_ flowwalk.Ctx, step *schema.Step, child *parserpkg.ParsedRunbook) (bool, error) {
	return visitor.enterInclude(step, child)
}

func (visitor *debugRunbookHashVisitor) enterInclude(step *schema.Step, child *parserpkg.ParsedRunbook) (bool, error) {
	if child == nil {
		return false, nil
	}
	if step != nil && step.IncludeSpec != nil {
		for childName, declaration := range child.Runbook.Inputs {
			if declaration == nil || declaration.Type != "secret" {
				continue
			}
			template, ok := step.IncludeSpec.Include.With[childName]
			if !ok {
				continue
			}
			names, all, err := internaldebugprotect.TemplateVariables(template)
			if err != nil || all {
				visitor.protectAllVars = true
				continue
			}
			for _, name := range names {
				visitor.protectedVars[name] = true
			}
		}
	}
	if err := visitor.appendRunbook(child); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Server) debugRunbookClosureHash(ctx context.Context, runbookPath string) (string, error) {
	hash, _, _, err := s.debugRunbookClosureMetadata(ctx, runbookPath)
	return hash, err
}

func (s *Server) debugRunbookClosureMetadata(ctx context.Context, runbookPath string) (string, map[string]bool, bool, error) {
	root, err := s.parser.Parse(ctx, runbookPath)
	if err != nil {
		return "", nil, false, err
	}
	visitor := &debugRunbookHashVisitor{}
	walker := &flowwalk.Walker{Loader: &serveLoader{p: s.parser}, MaxDepth: 32}
	if err := walker.Walk(ctx, root, visitor); err != nil {
		return "", nil, false, err
	}
	hash := sha256.New()
	for _, entry := range visitor.entries {
		_, _ = hash.Write(entry)
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), visitor.protectedVars, visitor.protectAllVars, nil
}
