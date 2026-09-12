package presentation

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type fileSnapshot struct {
	path      string
	data      []byte
	missing   bool
	directory bool
}
type filesystem struct {
	ctx     context.Context
	buffers map[string]Buffer
	reads   map[string]fileSnapshot
	deps    map[string]Dependency
	bytes   int
}

func pathKey(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else {
		parent := filepath.Dir(path)
		parts := []string{filepath.Base(path)}
		for parent != filepath.Dir(parent) {
			if resolved, err := filepath.EvalSymlinks(parent); err == nil {
				path = resolved
				for i := len(parts) - 1; i >= 0; i-- {
					path = filepath.Join(path, parts[i])
				}
				break
			}
			parts = append(parts, filepath.Base(parent))
			parent = filepath.Dir(parent)
		}
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
func FileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
func validateBuffer(b Buffer) bool {
	return validateBufferWithPathKey(b, pathKey)
}

func lexicalPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func validateBufferWithPathKey(b Buffer, key func(string) string) bool {
	if !filepath.IsAbs(b.Path) || b.URI == "" || b.Version < 0 || b.Version > 9007199254740991 {
		return false
	}
	u, err := url.Parse(b.URI)
	if err != nil || u.Scheme == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "file") {
		p := filepath.FromSlash(u.Path)
		if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '\\' && p[2] == ':' {
			p = p[1:]
		}
		if u.Host != "" && u.Host != "localhost" {
			p = `\\` + u.Host + p
		}
		if key(p) != key(b.Path) {
			return false
		}
	}
	return true
}
func (f *filesystem) read(path string) ([]byte, error) {
	if f.ctx != nil && f.ctx.Err() != nil {
		return nil, f.ctx.Err()
	}
	key := pathKey(path)
	base := strings.ToLower(filepath.Base(path))
	resolvedBase := strings.ToLower(filepath.Base(key))
	if base == ".env" || strings.HasPrefix(base, ".env.") || resolvedBase == ".env" || strings.HasPrefix(resolvedBase, ".env.") || f.ctx != nil && (base == "diff.txt" || resolvedBase == "diff.txt") {
		return nil, errors.New("restricted dependency")
	}
	if b, ok := f.buffers[key]; ok {
		v := b.Version
		f.deps[key] = Dependency{URI: b.URI, Version: &v, Digest: Digest([]byte(b.Text))}
		return []byte(b.Text), nil
	}
	if s, ok := f.reads[key]; ok {
		if s.missing {
			return nil, os.ErrNotExist
		}
		return s.data, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			f.reads[key] = fileSnapshot{path: path, missing: true}
			f.deps[key] = Dependency{URI: FileURI(path), Missing: true}
		}
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	f.bytes += len(data)
	if len(data) > MaxBytes || f.bytes > MaxBytes*4 || len(f.reads) >= MaxEntries {
		return nil, errors.New("limit-exceeded")
	}
	f.reads[key] = fileSnapshot{path: path, data: data}
	f.deps[key] = Dependency{URI: FileURI(path), Digest: Digest(data)}
	return data, nil
}

type virtualInfo struct {
	path string
	size int64
	dir  bool
}

func (v virtualInfo) Name() string { return filepath.Base(v.path) }
func (v virtualInfo) Size() int64  { return v.size }
func (v virtualInfo) Mode() fs.FileMode {
	if v.dir {
		return fs.ModeDir | 0700
	}
	return 0600
}
func (v virtualInfo) ModTime() time.Time { return time.Time{} }
func (v virtualInfo) IsDir() bool        { return v.dir }
func (v virtualInfo) Sys() any           { return nil }
func (f *filesystem) stat(path string) (fs.FileInfo, error) {
	if f.ctx != nil && f.ctx.Err() != nil {
		return nil, f.ctx.Err()
	}
	key := pathKey(path)
	if b, ok := f.buffers[key]; ok {
		return virtualInfo{path: path, size: int64(len(b.Text))}, nil
	}
	info, err := os.Stat(path)
	if err == nil {
		return info, nil
	}
	for k := range f.buffers {
		if strings.HasPrefix(k, key+string(filepath.Separator)) {
			if os.IsNotExist(err) {
				f.deps[key] = Dependency{URI: FileURI(path), Missing: true}
				f.reads[key] = fileSnapshot{path: path, missing: true}
			}
			return virtualInfo{path: path, dir: true}, nil
		}
	}
	if os.IsNotExist(err) {
		f.deps[key] = Dependency{URI: FileURI(path), Missing: true}
		f.reads[key] = fileSnapshot{path: path, missing: true}
	}
	return nil, err
}
func directoryDigest(path string) ([]byte, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	return digestDirectoryEntries(entries), nil
}
func digestDirectoryEntries(entries []fs.DirEntry) []byte {
	var b strings.Builder
	for _, e := range entries {
		fmtName := e.Name()
		b.WriteString(fmtName)
		b.WriteByte(0)
		b.WriteString(e.Type().String())
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
func authoringDirectoryDigest(path string) ([]byte, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(MaxEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > MaxEntries {
		return nil, errors.New("limit-exceeded")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return digestDirectoryEntries(entries), nil
}
func (f *filesystem) walk(root string, fn fs.WalkDirFunc) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if f.ctx != nil && f.ctx.Err() != nil {
			return f.ctx.Err()
		}
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		seen[pathKey(path)] = true
		if f.ctx != nil && len(seen) > MaxEntries {
			return errors.New("limit-exceeded")
		}
		if d.IsDir() {
			var data []byte
			var e error
			if f.ctx != nil {
				data, e = authoringDirectoryDigest(path)
			} else {
				data, e = directoryDigest(path)
			}
			if e != nil {
				return e
			}
			key := pathKey(path)
			f.reads[key] = fileSnapshot{path: path, data: data, directory: true}
			f.deps[key] = Dependency{URI: FileURI(path), Digest: Digest(data)}
			if len(seen) > MaxEntries {
				return errors.New("limit-exceeded")
			}
		}
		return fn(path, d, nil)
	})
	if err != nil {
		return err
	}
	paths := []string{}
	for k, b := range f.buffers {
		if !seen[k] && strings.HasPrefix(k, pathKey(root)+string(filepath.Separator)) {
			paths = append(paths, b.Path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if f.ctx != nil && (f.ctx.Err() != nil || len(seen)+len(paths) > MaxEntries) {
			return errors.New("limit-exceeded")
		}
		b := f.buffers[pathKey(path)]
		if err := fn(path, fs.FileInfoToDirEntry(virtualInfo{path: path, size: int64(len(b.Text))}), nil); err != nil {
			return err
		}
	}
	return nil
}
func (f *filesystem) dependencies() []Dependency {
	out := make([]Dependency, 0, len(f.deps))
	for _, d := range f.deps {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}
func (f *filesystem) stale() bool {
	if f.ctx != nil && f.ctx.Err() != nil {
		return true
	}
	for key, b := range f.buffers {
		if pathKey(b.Path) != key {
			return true
		}
	}
	for key, s := range f.reads {
		if f.ctx != nil && f.ctx.Err() != nil {
			return true
		}
		if pathKey(s.path) != key {
			return true
		}
		if _, ok := f.buffers[key]; ok {
			continue
		}
		if s.missing {
			if _, e := os.Stat(s.path); !os.IsNotExist(e) {
				return true
			}
			continue
		}
		var data []byte
		var err error
		if s.directory {
			if f.ctx != nil {
				data, err = authoringDirectoryDigest(s.path)
			} else {
				data, err = directoryDigest(s.path)
			}
		} else {
			file, e := os.Open(s.path)
			if e != nil {
				return true
			}
			data, err = io.ReadAll(io.LimitReader(file, MaxBytes+1))
			file.Close()
		}
		if err != nil || Digest(data) != Digest(s.data) {
			return true
		}
	}
	return false
}
