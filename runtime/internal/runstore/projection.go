package runstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const (
	runProjectionSchemaV1 = "yawr.run-projection/v1"
	maxRunProjectionFiles = 10_000
	maxRunProjectionBytes = 512 << 20
	maxRunProjectionFile  = 256 << 20
)

type runProjectionArchiveV1 struct {
	SchemaVersion string                `json:"schema_version"`
	RunID         string                `json:"run_id"`
	Files         []runProjectionFileV1 `json:"files"`
}

type runProjectionFileV1 struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Data   []byte `json:"data"`
}

func (store *DirRunStore) ExportRunProjection(ctx context.Context, runID string) (json.RawMessage, error) {
	store.mutationMu.RLock()
	defer store.mutationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	root := store.RunDir(runID)
	currentSnapshot, referencedBlobs, err := currentRunProjectionState(root)
	if err != nil {
		return nil, err
	}
	files := make([]runProjectionFileV1, 0)
	total := int64(0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("runstore: projection contains unsupported file %s", path)
		}
		if entry.Name() == ".writer.lock" || entry.Name() == ".writer.epoch" || strings.HasSuffix(entry.Name(), ".tmp") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !validProjectionPath(relative) {
			return fmt.Errorf("runstore: invalid projection path %q", relative)
		}
		if currentSnapshot != "" &&
			(strings.HasPrefix(relative, "snapshots/") && relative != currentSnapshot ||
				strings.HasPrefix(relative, "blobs/") && !referencedBlobs[relative]) {
			return nil
		}
		data, err := readFileBounded(path, maxRunProjectionFile)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > maxRunProjectionBytes || len(files) >= maxRunProjectionFiles {
			return errors.New("runstore: run projection exceeds archive limits")
		}
		digest := sha256.Sum256(data)
		files = append(files, runProjectionFileV1{
			Path: relative, Digest: fmt.Sprintf("sha256:%x", digest), Data: data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("runstore: run projection is empty")
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	encoded, err := json.Marshal(runProjectionArchiveV1{
		SchemaVersion: runProjectionSchemaV1, RunID: runID, Files: files,
	})
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxRunProjectionBytes*2 {
		return nil, errors.New("runstore: encoded run projection exceeds archive limit")
	}
	return encoded, nil
}

func currentRunProjectionState(root string) (string, map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, "snapshots"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	latestCheckpoint := ""
	latestCheckpointSequence := int64(0)
	latestStep := ""
	latestStepIndex := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if strings.HasPrefix(entry.Name(), "checkpoint-") {
			sequence, parseErr := parseCheckpointSequence(entry.Name())
			if parseErr == nil && (latestCheckpoint == "" || sequence > latestCheckpointSequence) {
				latestCheckpoint, latestCheckpointSequence = entry.Name(), sequence
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), "step-") {
			index, parseErr := parseSnapshotIndex(entry.Name())
			if parseErr == nil && (latestStep == "" || index > latestStepIndex) {
				latestStep, latestStepIndex = entry.Name(), index
			}
		}
	}
	latest := latestCheckpoint
	if latest == "" {
		latest = latestStep
	}
	if latest == "" {
		return "", nil, nil
	}
	data, err := readFileBounded(filepath.Join(root, "snapshots", latest), maxRunStateBytes)
	if err != nil {
		return "", nil, err
	}
	var snapshot any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return "", nil, fmt.Errorf("runstore: decode current projection snapshot: %w", err)
	}
	referenced := make(map[string]bool)
	collectProjectionBlobPaths(snapshot, referenced)
	return "snapshots/" + latest, referenced, nil
}

func collectProjectionBlobPaths(value any, paths map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "digest" {
				if digest, ok := nested.(string); ok && validSHA256Digest(digest) {
					paths["blobs/"+strings.TrimPrefix(digest, "sha256:")+".json.gz"] = true
				}
			}
			collectProjectionBlobPaths(nested, paths)
		}
	case []any:
		for _, nested := range typed {
			collectProjectionBlobPaths(nested, paths)
		}
	}
}

func (store *DirRunStore) RestoreRunProjection(ctx context.Context, runID string, encoded json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	if len(encoded) == 0 || len(encoded) > maxRunProjectionBytes*2 {
		return errors.New("runstore: invalid run projection size")
	}
	var archive runProjectionArchiveV1
	if err := decodeJSONDocument(encoded, &archive); err != nil {
		return fmt.Errorf("runstore: decode run projection: %w", err)
	}
	if archive.SchemaVersion != runProjectionSchemaV1 || archive.RunID != runID ||
		len(archive.Files) == 0 || len(archive.Files) > maxRunProjectionFiles {
		return errors.New("runstore: invalid run projection header")
	}
	total := int64(0)
	previousPath := ""
	for _, file := range archive.Files {
		if !validProjectionPath(file.Path) || file.Path <= previousPath || len(file.Data) > maxRunProjectionFile {
			return errors.New("runstore: invalid run projection file")
		}
		total += int64(len(file.Data))
		if total > maxRunProjectionBytes {
			return errors.New("runstore: run projection exceeds archive limits")
		}
		digest := sha256.Sum256(file.Data)
		if file.Digest != fmt.Sprintf("sha256:%x", digest) {
			return fmt.Errorf("runstore: run projection file %q digest mismatch", file.Path)
		}
		previousPath = file.Path
	}

	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	if err := os.MkdirAll(store.baseDir, 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(store.writerLockPath(runID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	acquired, err := tryLockRunLeaseFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return err
	}
	if !acquired {
		_ = lockFile.Close()
		return engine.ErrRunLeaseHeld
	}
	defer func() {
		_ = unlockRunLeaseFile(lockFile)
		_ = lockFile.Close()
	}()

	temporary, err := os.MkdirTemp(store.baseDir, ".run-projection-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	for _, archived := range archive.Files {
		path := filepath.Join(temporary, filepath.FromSlash(archived.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := file.Write(archived.Data); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	tracePath := filepath.Join(temporary, "trace.jsonl")
	if _, err := os.Stat(tracePath); errors.Is(err, os.ErrNotExist) {
		traceFile, createErr := os.OpenFile(tracePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return createErr
		}
		if syncErr := traceFile.Sync(); syncErr != nil {
			_ = traceFile.Close()
			return syncErr
		}
		if closeErr := traceFile.Close(); closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return err
	}
	if err := syncDirectory(temporary); err != nil {
		return err
	}

	store.mu.Lock()
	writer := store.writers[runID]
	delete(store.writers, runID)
	delete(store.plans, runID)
	delete(store.planDigests, runID)
	store.mu.Unlock()
	if writer != nil {
		if err := writer.Close(); err != nil {
			return err
		}
	}
	target := store.RunDir(runID)
	backup := target + ".restore-backup-" + uuid.NewString()
	targetExists := false
	if _, err := os.Stat(target); err == nil {
		targetExists = true
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		if targetExists {
			_ = os.Rename(backup, target)
		}
		return err
	}
	if targetExists {
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
	}
	return syncDirectory(store.baseDir)
}

func validProjectionPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
