package sessionstore

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

var errImmutableFileConflict = errors.New("sessionstore: immutable file conflicts with existing content")

func publishImmutableFile(path string, data []byte, mode fs.FileMode, maximum int64) error {
	for range 2 {
		existing, err := readFileBounded(path, maximum)
		if err == nil {
			if bytes.Equal(existing, data) {
				return nil
			}
			if len(existing) == 0 {
				return repairTornImmutableFile(path, data, mode, maximum)
			}
			return errImmutableFileConflict
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := publishImmutableFileExclusive(path, data, mode); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrExist) && !os.IsExist(err) {
			return err
		}
	}
	return errImmutableFileConflict
}

func publishImmutableFileExclusive(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".immutable-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func repairTornImmutableFile(path string, data []byte, mode fs.FileMode, maximum int64) error {
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".repair.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	acquired, err := tryLockSessionFile(lockFile)
	if err != nil {
		_ = lockFile.Close()
		return err
	}
	if !acquired {
		_ = lockFile.Close()
		return errors.New("sessionstore: immutable file repair is already in progress")
	}
	defer func() {
		_ = unlockSessionFile(lockFile)
		_ = lockFile.Close()
	}()
	existing, err := readFileBounded(path, maximum)
	if err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && len(existing) != 0 {
		return errImmutableFileConflict
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return publishImmutableFileExclusive(path, data, mode)
}
