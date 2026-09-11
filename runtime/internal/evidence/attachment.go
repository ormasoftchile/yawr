package evidence

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
)

// AttachmentStore copies files into the run's attachments/ directory,
// deduplicating by SHA256 content hash.
type AttachmentStore struct {
	dir string // .runbook/runs/<run-id>/attachments/
}

// NewAttachmentStore constructs an AttachmentStore rooted at runDir/attachments.
func NewAttachmentStore(runDir string) *AttachmentStore {
	base := runDir
	if base == "" {
		base = "."
	}
	dir := filepath.Join(base, "attachments")
	_ = os.MkdirAll(dir, 0o755)
	return &AttachmentStore{dir: dir}
}

// Store copies a file into the attachment directory and returns an EvidenceRecord.
// Files with identical content (same SHA256) share one copy.
func (s *AttachmentStore) Store(sourcePath string) (*evidencepkg.EvidenceRecord, error) {
	sha, size, err := evidencepkg.HashFile(sourcePath)
	if err != nil {
		return nil, err
	}

	ext := filepath.Ext(sourcePath)
	destName := sha + ext
	destPath := filepath.Join(s.dir, destName)

	if _, err := os.Stat(destPath); err == nil {
		return &evidencepkg.EvidenceRecord{
			Name:   filepath.Base(sourcePath),
			Kind:   evidencepkg.EvidenceKindAttachment,
			Path:   filepath.Join("attachments", destName),
			SHA256: sha,
			Size:   size,
		}, nil
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("evidence: open source %s: %w", sourcePath, err)
	}
	defer src.Close()

	// Atomic write: temp → sync → rename ensures no partial files at destPath.
	tmpPath := destPath + ".tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("evidence: create attachment tmp %s: %w", tmpPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("evidence: rename attachment %s: %w", destPath, err)
	}

	return &evidencepkg.EvidenceRecord{
		Name:   filepath.Base(sourcePath),
		Kind:   evidencepkg.EvidenceKindAttachment,
		Path:   filepath.Join("attachments", destName),
		SHA256: sha,
		Size:   size,
	}, nil
}
