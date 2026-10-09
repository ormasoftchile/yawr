package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// FileDispatchStore implements StandaloneDispatchStore with persistent disk storage,
// atomic file operations, cross-process locking, crash recovery, and idempotency conflict detection.
type FileDispatchStore struct {
	dir        string
	mu         sync.Mutex // in-process mutex
	locksDir   string
	recordsDir string
	keysDir    string
}

// storedRecord wraps the dispatch state and serialized result on disk.
type storedRecord struct {
	State       engine.DispatchState `json:"state"`
	RenderedReq any                  `json:"rendered_req,omitempty"`
	Result      any                  `json:"result,omitempty"`
}

// NewFileDispatchStore constructs a new persistent FileDispatchStore in dir.
func NewFileDispatchStore(dir string) (*FileDispatchStore, error) {
	if dir == "" {
		return nil, errors.New("dispatch store: directory path cannot be empty")
	}
	locksDir := filepath.Join(dir, "locks")
	recordsDir := filepath.Join(dir, "records")
	keysDir := filepath.Join(dir, "keys")

	for _, d := range []string{dir, locksDir, recordsDir, keysDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("dispatch store: mkdir %s: %w", d, err)
		}
	}

	return &FileDispatchStore{
		dir:        dir,
		locksDir:   locksDir,
		recordsDir: recordsDir,
		keysDir:    keysDir,
	}, nil
}

func (s *FileDispatchStore) acquireLock(ctx context.Context, keyHash string) (*os.File, error) {
	lockPath := filepath.Join(s.locksDir, keyHash+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dispatch: open lockfile %s: %w", lockPath, err)
	}

	for {
		acquired, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("dispatch: acquire lock %s: %w", lockPath, err)
		}
		if acquired {
			return f, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *FileDispatchStore) releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	defer f.Close()
	return unlockFile(f)
}

func (s *FileDispatchStore) atomicWriteJSON(path string, val any) error {
	data, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PrepareDispatch records a durable intent before provider I/O.
func (s *FileDispatchStore) PrepareDispatch(ctx context.Context, request engine.DispatchRequest) (engine.DispatchState, error) {
	if err := ctx.Err(); err != nil {
		return engine.DispatchState{}, err
	}

	classification := request.Classification
	if classification == "" {
		classification = "unspecified"
	}

	rendered, err := json.Marshal(request.RenderedRequest)
	if err != nil {
		return engine.DispatchState{}, fmt.Errorf("dispatch: encode rendered request: %w", err)
	}
	requestDigest := engine.InteractionPayloadDigest(rendered)

	// Derive or accept idempotency key
	idempotencyKey := ""
	if reqMap, ok := request.RenderedRequest.(map[string]any); ok {
		if rawKey, hasKey := reqMap["idempotency_key"].(string); hasKey && rawKey != "" {
			idempotencyKey = rawKey
		}
	}
	if idempotencyKey == "" {
		idempotencyKey = engine.InteractionPayloadDigest([]byte("adhoc.dispatch/v1\x00" + requestDigest))
	}

	keyHashBytes := sha256.Sum256([]byte(idempotencyKey))
	keyHash := hex.EncodeToString(keyHashBytes[:16])

	s.mu.Lock()
	defer s.mu.Unlock()

	lockFile, err := s.acquireLock(ctx, keyHash)
	if err != nil {
		return engine.DispatchState{}, err
	}
	defer s.releaseLock(lockFile)

	keyPath := filepath.Join(s.keysDir, keyHash+".json")
	if existingData, err := os.ReadFile(keyPath); err == nil {
		var existing storedRecord
		if err := json.Unmarshal(existingData, &existing); err == nil {
			// 1. Idempotency Conflict Check:
			// Reusing an idempotency key with different arguments must be rejected!
			if existing.State.RequestDigest != requestDigest {
				return existing.State, fmt.Errorf("%w: idempotency key %q was previously used with different request parameters", toolpkg.ErrIdempotencyConflict, idempotencyKey)
			}

			// 2. Already Settled (Replay):
			if existing.State.Status == engine.DispatchStatusSettled {
				return existing.State, nil
			}

			// 3. Interrupted Prior Execution (Crash Recovery):
			if existing.State.Status == engine.DispatchStatusPrepared {
				existing.State.Status = engine.DispatchStatusIndeterminate
				_ = s.atomicWriteJSON(keyPath, existing)
				occPath := filepath.Join(s.recordsDir, existing.State.OccurrenceID+".json")
				_ = s.atomicWriteJSON(occPath, existing)
				return existing.State, fmt.Errorf("%w: unmatched dispatch intent %s: previous execution was interrupted before settlement", engine.ErrIndeterminate, existing.State.OccurrenceID)
			}

			// 4. Already Indeterminate:
			if existing.State.Status == engine.DispatchStatusIndeterminate {
				return existing.State, fmt.Errorf("%w: unmatched dispatch intent %s", engine.ErrIndeterminate, existing.State.OccurrenceID)
			}
		}
	}

	// New Dispatch
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	occIDBytes := sha256.Sum256([]byte(idempotencyKey + "\x00" + nowStr))
	occID := hex.EncodeToString(occIDBytes[:16])

	state := engine.DispatchState{
		SchemaVersion:                  engine.DispatchStateSchemaV1,
		OccurrenceID:                   occID,
		Classification:                 classification,
		EndpointIdentity:               request.EndpointIdentity,
		RequestDigest:                  requestDigest,
		IdempotencyKey:                 idempotencyKey,
		ProviderSupportsReconciliation: request.ProviderSupportsReconciliation,
		Status:                         engine.DispatchStatusPrepared,
		PreparedAt:                     nowStr,
	}

	rec := storedRecord{
		State:       state,
		RenderedReq: request.RenderedRequest,
	}

	if err := s.atomicWriteJSON(keyPath, rec); err != nil {
		return engine.DispatchState{}, fmt.Errorf("dispatch: write key record: %w", err)
	}
	occPath := filepath.Join(s.recordsDir, occID+".json")
	if err := s.atomicWriteJSON(occPath, rec); err != nil {
		return engine.DispatchState{}, fmt.Errorf("dispatch: write occurrence record: %w", err)
	}

	return state, nil
}

// SettleDispatch updates a dispatch to settled with the result digest.
func (s *FileDispatchStore) SettleDispatch(ctx context.Context, occurrenceID string, result any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	occPath := filepath.Join(s.recordsDir, occurrenceID+".json")
	data, err := os.ReadFile(occPath)
	if err != nil {
		return fmt.Errorf("dispatch: occurrence %s not found: %w", occurrenceID, err)
	}

	var rec storedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("dispatch: unmarshal occurrence %s: %w", occurrenceID, err)
	}

	keyHashBytes := sha256.Sum256([]byte(rec.State.IdempotencyKey))
	keyHash := hex.EncodeToString(keyHashBytes[:16])

	lockFile, err := s.acquireLock(ctx, keyHash)
	if err != nil {
		return err
	}
	defer s.releaseLock(lockFile)

	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("dispatch: marshal result for settle: %w", err)
	}

	rec.State.Status = engine.DispatchStatusSettled
	rec.State.ResultDigest = engine.InteractionPayloadDigest(payload)
	rec.State.SettledAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Result = result

	if err := s.atomicWriteJSON(occPath, rec); err != nil {
		return fmt.Errorf("dispatch: update occurrence %s: %w", occurrenceID, err)
	}
	keyPath := filepath.Join(s.keysDir, keyHash+".json")
	if err := s.atomicWriteJSON(keyPath, rec); err != nil {
		return fmt.Errorf("dispatch: update key record: %w", err)
	}
	return nil
}

// LookupDispatch returns a recorded dispatch by occurrence ID.
func (s *FileDispatchStore) LookupDispatch(ctx context.Context, occurrenceID string) (*engine.DispatchState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	occPath := filepath.Join(s.recordsDir, occurrenceID+".json")
	data, err := os.ReadFile(occPath)
	if err != nil {
		return nil, false
	}
	var rec storedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false
	}
	out := rec.State
	return &out, true
}

// LookupByIdempotencyKey returns a recorded dispatch by idempotency key.
func (s *FileDispatchStore) LookupByIdempotencyKey(ctx context.Context, key string) (*engine.DispatchState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	keyHashBytes := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(keyHashBytes[:16])

	keyPath := filepath.Join(s.keysDir, keyHash+".json")
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, false
	}
	var rec storedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false
	}
	out := rec.State
	return &out, true
}

// MarkIndeterminate flags a dispatch occurrence as indeterminate.
func (s *FileDispatchStore) MarkIndeterminate(ctx context.Context, occurrenceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	occPath := filepath.Join(s.recordsDir, occurrenceID+".json")
	data, err := os.ReadFile(occPath)
	if err != nil {
		return fmt.Errorf("dispatch: occurrence %s not found: %w", occurrenceID, err)
	}
	var rec storedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("dispatch: unmarshal occurrence %s: %w", occurrenceID, err)
	}

	keyHashBytes := sha256.Sum256([]byte(rec.State.IdempotencyKey))
	keyHash := hex.EncodeToString(keyHashBytes[:16])

	lockFile, err := s.acquireLock(ctx, keyHash)
	if err != nil {
		return err
	}
	defer s.releaseLock(lockFile)

	rec.State.Status = engine.DispatchStatusIndeterminate
	if err := s.atomicWriteJSON(occPath, rec); err != nil {
		return err
	}
	keyPath := filepath.Join(s.keysDir, keyHash+".json")
	return s.atomicWriteJSON(keyPath, rec)
}

// SettledOutcomeFor returns the replayable outcome of a settled occurrence.
func (s *FileDispatchStore) SettledOutcomeFor(ctx context.Context, occurrenceID string) (*engine.SettledOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	occPath := filepath.Join(s.recordsDir, occurrenceID+".json")
	data, err := os.ReadFile(occPath)
	if err != nil {
		return nil, false
	}
	var rec storedRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false
	}
	if rec.State.Status != engine.DispatchStatusSettled {
		return nil, false
	}
	outcome := &engine.SettledOutcome{}
	if rec.Result != nil {
		if m, ok := rec.Result.(map[string]any); ok {
			if errMsg, hasErr := m["error"].(string); hasErr {
				outcome.Error = errMsg
			}
			if out, hasOut := m["output"]; hasOut {
				b, _ := json.Marshal(out)
				outcome.Output = b
			}
			if stdout, hasStdout := m["stdout"].(string); hasStdout {
				outcome.Stdout = stdout
			}
			if stderr, hasStderr := m["stderr"].(string); hasStderr {
				outcome.Stderr = stderr
			}
			if exitCode, hasCode := m["exit_code"].(float64); hasCode {
				outcome.ExitCode = int(exitCode)
			}
		} else {
			b, err := json.Marshal(rec.Result)
			if err == nil {
				_ = json.Unmarshal(b, outcome)
			}
		}
	}
	return outcome, true
}

// LockKey acquires exclusive ownership of an idempotency key for the full
// prepare → execute → settle span.
func (s *FileDispatchStore) LockKey(ctx context.Context, key string) (func(), error) {
	keyHashBytes := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(keyHashBytes[:16])

	lockFile, err := s.acquireLock(ctx, keyHash+".span")
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = s.releaseLock(lockFile)
		})
	}, nil
}

