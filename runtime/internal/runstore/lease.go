package runstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

type fileRunLease struct {
	file      *os.File
	epoch     uint64
	runID     string
	owner     *DirRunStore
	once      sync.Once
	err       error
	onRelease func()
}

func (s *DirRunStore) AcquireRunLease(ctx context.Context, runID string) (engine.RunLease, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	if err := s.preflightPresentationLease(ctx, runID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.baseDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(s.writerLockPath(runID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	acquired, err := tryLockRunLeaseFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !acquired {
		_ = file.Close()
		return nil, engine.ErrRunLeaseHeld
	}
	if err := s.preflightPresentationLease(ctx, runID); err != nil {
		_ = unlockRunLeaseFile(file)
		_ = file.Close()
		return nil, err
	}
	directory := s.RunDir(runID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		_ = unlockRunLeaseFile(file)
		_ = file.Close()
		return nil, err
	}

	epoch, err := nextRunWriterEpoch(s.writerEpochPath(runID), filepath.Join(s.RunDir(runID), ".writer.epoch"))
	if err != nil {
		_ = unlockRunLeaseFile(file)
		_ = file.Close()
		return nil, err
	}
	lease := &fileRunLease{file: file, epoch: epoch, runID: runID, owner: s}
	lease.onRelease = func() {
		s.mu.Lock()
		delete(s.leases, lease)
		if s.leaseEpochs[runID] == epoch {
			delete(s.leaseEpochs, runID)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.leases[lease] = struct{}{}
	s.leaseEpochs[runID] = epoch
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		_ = lease.releaseLocked()
		return nil, err
	}
	return lease, nil
}

func (s *DirRunStore) preflightPresentationLease(ctx context.Context, runID string) error {
	if _, err := s.LoadPlan(ctx, runID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	_, err := s.LoadState(ctx, runID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *DirRunStore) writerLockPath(runID string) string {
	return filepath.Join(s.baseDir, "."+runID+".writer.lock")
}

func (s *DirRunStore) writerEpochPath(runID string) string {
	return filepath.Join(s.baseDir, "."+runID+".writer.epoch")
}

func (lease *fileRunLease) Epoch() uint64 {
	if lease == nil {
		return 0
	}
	return lease.epoch
}

func (lease *fileRunLease) Release() error {
	if lease == nil {
		return nil
	}
	if lease.owner != nil {
		lease.owner.mutationMu.Lock()
		defer lease.owner.mutationMu.Unlock()
	}
	return lease.releaseLocked()
}

func (lease *fileRunLease) releaseLocked() error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		if lease.file == nil {
			return
		}
		lease.err = errors.Join(unlockRunLeaseFile(lease.file), lease.file.Close())
		if lease.onRelease != nil {
			lease.onRelease()
		}
	})
	return lease.err
}

func nextRunWriterEpoch(path string, legacyPath string) (uint64, error) {
	current, found, err := readRunWriterEpoch(path)
	if err != nil {
		return 0, err
	}
	legacy, legacyFound, err := readRunWriterEpoch(legacyPath)
	if err != nil {
		return 0, err
	}
	if legacyFound && (!found || legacy > current) {
		current = legacy
	}
	if current == ^uint64(0) {
		return 0, errors.New("runstore: writer epoch exhausted")
	}
	next := current + 1
	if err := writeFileAtomic(path, []byte(strconv.FormatUint(next, 10)+"\n")); err != nil {
		return 0, err
	}
	return next, nil
}

func readRunWriterEpoch(path string) (uint64, bool, error) {
	data, err := readFileBounded(path, 32)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	text := strings.TrimSpace(string(data))
	epoch, err := strconv.ParseUint(text, 10, 64)
	if err != nil || text != strconv.FormatUint(epoch, 10) {
		return 0, false, errors.New("runstore: invalid writer epoch")
	}
	return epoch, true, nil
}

func (s *DirRunStore) validateWriterEpoch(runID string, expected uint64) error {
	current, found, err := readRunWriterEpoch(s.writerEpochPath(runID))
	if err != nil {
		return err
	}
	if !found {
		current, found, err = readRunWriterEpoch(filepath.Join(s.RunDir(runID), ".writer.epoch"))
	}
	if err != nil {
		return err
	}
	if !found {
		if expected == 0 {
			return nil
		}
		return engine.ErrRunLeaseStale
	}
	if expected == 0 || expected != current {
		return engine.ErrRunLeaseStale
	}
	s.mu.Lock()
	activeEpoch, active := s.leaseEpochs[runID]
	s.mu.Unlock()
	if !active || activeEpoch != expected {
		return engine.ErrRunLeaseStale
	}
	return nil
}
