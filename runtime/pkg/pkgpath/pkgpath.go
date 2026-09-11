// Package pkgpath implements the secure path resolution rules of
// Secure path resolution is applied
// uniformly to requires[].path, exports.tools[].path, execute.path,
// toolRefs[].path, tool-paths[], and package-internal include/import paths.
package pkgpath

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"golang.org/x/text/unicode/norm"
)

// Class distinguishes containment-root semantics per §Resolution base per
// path kind (Table tab:tool-path-bases).
type Class int

const (
	// PackageInternal paths (exports.tools[].path, execute.path, in-package
	// includes) MUST NOT escape the package root: violation is PKG-007.
	PackageInternal Class = iota
	// WorkspaceLevel paths (requires[].path, toolRefs[].path, tool-paths[])
	// are operator configuration and MAY legitimately resolve outside the
	// workspace root; escaping it is only reported (PKG-W003), never
	// rejected.
	WorkspaceLevel
)

// maxLinkHops bounds symlink/junction chain resolution (§Symlinks, junctions,
// reparse points): exceeding it is PKG-008.
const maxLinkHops = 8

var reservedDOSNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// disallowedSyntax matches UNC/extended-length/device-namespace prefixes,
// drive-relative paths, and other forms rejected everywhere regardless of
// class (§Syntax rules, rule 3).
var (
	uncOrExtendedRE = regexp.MustCompile(`^\\\\`)
	driveRelativeRE = regexp.MustCompile(`^[A-Za-z]:`)
	uriSchemeRE     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
)

// ValidateAuthoredSyntax checks the syntax rules that MUST be checked before
// touching the filesystem (§Syntax rules). allowAbsolute controls whether an
// absolute (POSIX-style, leading "/") path is permitted — true for
// workspace-level kinds, false for package-internal kinds.
func ValidateAuthoredSyntax(p string, allowAbsolute bool) error {
	if p == "" {
		return errkit.New("PKG-007", "path must not be empty")
	}
	if strings.ContainsRune(p, '\\') {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: backslash is not a valid separator in authored artefacts; use '/'", p))
	}
	if strings.ContainsAny(p, "\x00") || containsControlChar(p) {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: control characters are not permitted", p))
	}
	if uncOrExtendedRE.MatchString(p) {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: UNC/extended-length/device paths are rejected", p))
	}
	if driveRelativeRE.MatchString(p) {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: drive-relative paths are rejected", p))
	}
	if uriSchemeRE.MatchString(p) {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: URI schemes are rejected", p))
	}
	if strings.HasPrefix(p, "~") {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: '~' expansion is rejected", p))
	}
	if strings.Contains(p, "$") || strings.Contains(p, "%") {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: environment-variable expansion is rejected", p))
	}
	if strings.HasPrefix(p, "/") && !allowAbsolute {
		return errkit.New("PKG-007", fmt.Sprintf("path %q: absolute paths are not permitted in package-internal references", p))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if err := validateSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

func containsControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateSegment(seg string) error {
	if seg == "." || seg == ".." {
		// Navigation segments are handled by containment checking, not by
		// the reserved-name/trailing-dot rules below.
		return nil
	}
	base := seg
	if idx := strings.Index(base, "."); idx > 0 {
		base = base[:idx]
	}
	if reservedDOSNames[strings.ToUpper(base)] || reservedDOSNames[strings.ToUpper(seg)] {
		return errkit.New("PKG-018", fmt.Sprintf("path segment %q: reserved DOS device name", seg))
	}
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return errkit.New("PKG-018", fmt.Sprintf("path segment %q: trailing dot or space is rejected for portability", seg))
	}
	return nil
}

// NormalizeForComparison NFC-normalises p for cross-path comparison, per the
// rule that two exports differing only by Unicode normalisation form (or
// only by ASCII case, on case-insensitive filesystems) collide as PKG-018.
func NormalizeForComparison(p string, caseInsensitive bool) string {
	n := norm.NFC.String(p)
	if caseInsensitive {
		n = strings.ToLower(n)
	}
	return n
}

// Resolve resolves p (already syntax-validated) relative to referencingDir,
// resolves any symlinks/junctions in the result, and checks containment
// inside containmentRoot. class determines whether escaping containmentRoot
// is a hard error (PackageInternal, PKG-007) or merely reported to the
// caller as external (WorkspaceLevel — caller decides whether to emit
// PKG-W003). Returns the resolved, real absolute path.
func Resolve(referencingDir, p, containmentRoot string, class Class) (resolved string, external bool, err error) {
	joined := filepath.Join(referencingDir, filepath.FromSlash(p))
	real, err := resolveLinks(joined)
	if err != nil {
		return "", false, err
	}
	rootReal, rootErr := filepath.EvalSymlinks(containmentRoot)
	if rootErr != nil {
		// Containment root itself may not exist yet in some validation
		// paths (e.g. dry structural checks); fall back to the lexical form.
		rootReal = containmentRoot
	}
	rootReal, _ = filepath.Abs(rootReal)
	contained := isContained(real, rootReal)
	if !contained {
		if class == PackageInternal {
			return "", true, errkit.New("PKG-007", fmt.Sprintf("path %q resolves outside its containment root %q", p, containmentRoot))
		}
		return real, true, nil
	}
	return real, false, nil
}

// isContained reports whether child is real (rootReal) or nested under it,
// compared case-insensitively on case-insensitive filesystems (Windows).
func isContained(child, root string) bool {
	childNorm := normalizedAbs(child)
	rootNorm := normalizedAbs(root)
	if childNorm == rootNorm {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(childNorm, rootNorm+sep)
}

func normalizedAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	n := norm.NFC.String(abs)
	if os.PathSeparator == '\\' { // Windows: case-insensitive filesystem
		n = strings.ToLower(n)
	}
	return filepath.Clean(n)
}

// resolveLinks resolves symlinks/junctions/reparse points in path using the
// host OS's own link-following semantics (this includes Windows junctions
// and reparse points, and any 8.3-short-name canonicalisation the OS
// performs as part of that). Before doing so, checkLinkHopBound walks
// path's own leaf symlink chain explicitly, counting hops, and fails
// closed with PKG-008 once maxLinkHops is exceeded (§6.3 rule 3) rather
// than relying entirely on the host OS's own ELOOP/"too many links"
// ceiling, which is unspecified and typically much larger than 8. A path
// that does not (yet) exist is not an error at this layer — it is
// returned in cleaned lexical form so callers can decide whether
// non-existence itself is an error (e.g. PKG-010).
func resolveLinks(path string) (string, error) {
	cur := filepath.Clean(path)
	if err := checkLinkHopBound(cur); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(cur)
	if err == nil {
		return real, nil
	}
	if os.IsNotExist(err) {
		return cur, nil
	}
	return "", errkit.New("PKG-008", fmt.Sprintf("cannot resolve symlink/junction chain for %q: %v", path, err))
}

// checkLinkHopBound walks path's own symlink/junction chain (path itself,
// then whatever it points to, then whatever that points to, ...), counting
// hops explicitly, and returns PKG-008 once maxLinkHops is exceeded. It
// intentionally stops (returning nil, deferring to resolveLinks/the OS) on
// any Lstat/Readlink error other than the chain simply terminating at a
// non-symlink, since those cases (missing file, permission error, etc.)
// are not this function's concern.
func checkLinkHopBound(path string) error {
	cur := path
	hops := 0
	for {
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		hops++
		if hops > maxLinkHops {
			return errkit.New("PKG-008", fmt.Sprintf(
				"symlink/junction chain for %q exceeds the maximum of %d hops", path, maxLinkHops))
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
	}
}
