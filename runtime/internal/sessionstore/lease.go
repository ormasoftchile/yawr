package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ormasoftchile/yawr/runtime/pkg/session"
)

type fileSessionLease struct {
	file      *os.File
	epoch     uint64
	sessionID string
	owner     *DirStore
	once      sync.Once
	err       error
}

func (store *DirStore) AcquireSessionLease(ctx context.Context, sessionID string) (session.Lease, error) {
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUUID("session id", sessionID); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(store.baseDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(store.writerLockPath(sessionID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	acquired, err := tryLockSessionFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !acquired {
		_ = file.Close()
		return nil, session.ErrSessionLeaseHeld
	}
	store.invalidateJournalCache(sessionID)
	headEpoch := uint64(0)
	if head, headErr := store.loadJournalHead(sessionID); headErr == nil {
		headEpoch = head.WriterEpoch
	} else if !errors.Is(headErr, os.ErrNotExist) {
		_ = unlockSessionFile(file)
		_ = file.Close()
		return nil, headErr
	}
	epoch, err := nextSessionWriterEpoch(store.writerEpochDir(sessionID), headEpoch)
	if err != nil {
		_ = unlockSessionFile(file)
		_ = file.Close()
		return nil, err
	}
	lease := &fileSessionLease{file: file, epoch: epoch, sessionID: sessionID, owner: store}
	store.mu.Lock()
	store.leases[lease] = struct{}{}
	store.leaseEpochs[sessionID] = epoch
	store.mu.Unlock()
	return lease, nil
}

func (lease *fileSessionLease) Epoch() uint64 {
	if lease == nil {
		return 0
	}
	return lease.epoch
}

func (lease *fileSessionLease) Advance() (uint64, error) {
	if lease == nil || lease.owner == nil {
		return 0, session.ErrSessionLeaseStale
	}
	lease.owner.mutationMu.Lock()
	defer lease.owner.mutationMu.Unlock()
	lease.owner.mu.Lock()
	active := lease.owner.leaseEpochs[lease.sessionID]
	lease.owner.mu.Unlock()
	if active != lease.epoch {
		return 0, session.ErrSessionLeaseStale
	}
	next, err := nextSessionWriterEpoch(lease.owner.writerEpochDir(lease.sessionID), lease.epoch)
	if err != nil {
		return 0, err
	}
	lease.epoch = next
	lease.owner.mu.Lock()
	lease.owner.leaseEpochs[lease.sessionID] = next
	lease.owner.mu.Unlock()
	lease.owner.invalidateJournalCache(lease.sessionID)
	return next, nil
}

func (lease *fileSessionLease) Release() error {
	if lease == nil {
		return nil
	}
	lease.owner.mutationMu.Lock()
	defer lease.owner.mutationMu.Unlock()
	return lease.releaseLocked()
}

func (lease *fileSessionLease) releaseLocked() error {
	lease.once.Do(func() {
		lease.err = errors.Join(unlockSessionFile(lease.file), lease.file.Close())
		lease.owner.mu.Lock()
		delete(lease.owner.leases, lease)
		if lease.owner.leaseEpochs[lease.sessionID] == lease.epoch {
			delete(lease.owner.leaseEpochs, lease.sessionID)
		}
		lease.owner.mu.Unlock()
		lease.owner.invalidateJournalCache(lease.sessionID)
	})
	return lease.err
}

func (store *DirStore) writerLockPath(sessionID string) string {
	return filepath.Join(store.baseDir, "."+sessionID+".writer.lock")
}

func (store *DirStore) writerEpochDir(sessionID string) string {
	return filepath.Join(store.baseDir, "."+sessionID+".writer-epochs")
}

func nextSessionWriterEpoch(directory string, minimum uint64) (uint64, error) {
	current, err := highestSessionWriterEpoch(directory)
	if err != nil {
		return 0, err
	}
	if minimum > current {
		current = minimum
	}
	if current == ^uint64(0) {
		return 0, errors.New("sessionstore: writer epoch exhausted")
	}
	next := current + 1
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return 0, err
	}
	path := filepath.Join(directory, fmt.Sprintf("epoch-%020d", next))
	if err := publishImmutableFile(path, []byte(strconv.FormatUint(next, 10)+"\n"), 0o600, 32); err != nil {
		return 0, err
	}
	return next, nil
}

func (store *DirStore) validateWriterEpoch(sessionID string, expected uint64) error {
	current, err := highestSessionWriterEpoch(store.writerEpochDir(sessionID))
	if err != nil {
		return session.ErrSessionLeaseStale
	}
	if expected == 0 || current != expected {
		return session.ErrSessionLeaseStale
	}
	store.mu.Lock()
	active := store.leaseEpochs[sessionID]
	store.mu.Unlock()
	if active != expected {
		return session.ErrSessionLeaseStale
	}
	return nil
}

func highestSessionWriterEpoch(directory string) (uint64, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var highest uint64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "epoch-") {
			continue
		}
		text := strings.TrimPrefix(entry.Name(), "epoch-")
		epoch, parseErr := strconv.ParseUint(text, 10, 64)
		if parseErr != nil || text != fmt.Sprintf("%020d", epoch) {
			return 0, errors.New("sessionstore: invalid writer epoch generation")
		}
		if epoch > highest {
			highest = epoch
		}
	}
	return highest, nil
}
