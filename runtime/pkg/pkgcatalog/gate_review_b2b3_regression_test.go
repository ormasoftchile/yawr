package pkgcatalog

import (
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// TestBuild_DigestClosure_SubstituteRunbookByteChange_ChangesPackageDigest
// is a B2 regression test (Barbara's gate review): the package digest
// closure (design/yawr/sections/06-tool-runtime.tex §7.2) MUST include
// every substitute runbook an exported tool's execute.kind: runbook
// action points at -- a byte-for-byte change to that substitute file,
// with every other package byte held constant, MUST change both the
// package digest and (since B3 requires tier-1 catalog entries to carry
// the package digest) the tool's catalog entry digest and the overall
// catalog digest.
func TestBuild_DigestClosure_SubstituteRunbookByteChange_ChangesPackageDigest(t *testing.T) {
	ws := t.TempDir()
	pkgRoot := filepath.Join(ws, "vendor", "acme-subst-tools")

	writeFile(t, filepath.Join(pkgRoot, "yawr-package.yaml"), `apiVersion: yawr.tool-package/v1
meta:
  name: acme.subst-tools
  version: "1.0.0"
exports:
  tools:
    - id: diagnostics
      path: tools/diagnostics.tool.yaml
`)
	writeFile(t, filepath.Join(pkgRoot, "tools", "diagnostics.tool.yaml"), `apiVersion: yawr.tool/v1
meta:
  name: diagnostics
  version: "1.0.0"
transport:
  mode: native
  command: does-not-run
actions:
  - name: diagnose
    description: substituted action
    args:
      name:
        type: string
        required: true
    outputs:
      summary:
        type: string
    execute:
      kind: runbook
      path: ../runbooks/diagnose.runbook.yaml
`)
	substPath := filepath.Join(pkgRoot, "runbooks", "diagnose.runbook.yaml")
	writeFile(t, substPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: acme.subst-tools/diagnose
name: diagnose substitute v1
inputs:
  name:
    type: string
    required: true
    from: context
outputs:
  summary:
    type: string
    value: "diagnosed-${name}"
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	buildOpts := func() BuildOptions {
		return BuildOptions{
			WorkspaceRoot: ws,
			ProjectRequires: []*schema.PackageRequirement{
				{Package: "acme.subst-tools", Version: "^1.0.0", Path: "vendor/acme-subst-tools"},
			},
		}
	}

	catBefore, errs := Build(buildOpts())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors (before): %v", errs)
	}
	entryBefore, ok := catBefore.ByQualified("acme.subst-tools/diagnostics")
	if !ok {
		t.Fatal("expected qualified entry acme.subst-tools/diagnostics (before)")
	}
	digestBefore := entryBefore.Digest
	catalogDigestBefore := catBefore.CatalogDigest()
	if digestBefore == "" || catalogDigestBefore == "" {
		t.Fatalf("expected non-empty digests, got entry=%q catalog=%q", digestBefore, catalogDigestBefore)
	}

	// Change only the substitute runbook's bytes (a cosmetic display name
	// edit, no structural change to the tool file or manifest) and rebuild.
	writeFile(t, substPath, `$schema: "https://schemas.yawr.dev/yawr.runbook/v1.json"
apiVersion: yawr.runbook/v1
id: acme.subst-tools/diagnose
name: diagnose substitute v2 -- changed bytes only
inputs:
  name:
    type: string
    required: true
    from: context
outputs:
  summary:
    type: string
    value: "diagnosed-${name}"
flow:
  - step:
      id: end
      type: end
      outcome: {category: success, code: ok}
`)

	catAfter, errs := Build(buildOpts())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors (after): %v", errs)
	}
	entryAfter, ok := catAfter.ByQualified("acme.subst-tools/diagnostics")
	if !ok {
		t.Fatal("expected qualified entry acme.subst-tools/diagnostics (after)")
	}
	digestAfter := entryAfter.Digest
	catalogDigestAfter := catAfter.CatalogDigest()

	if digestBefore == digestAfter {
		t.Fatalf("package digest did not change after editing the substitute runbook's bytes: both %q", digestBefore)
	}
	if catalogDigestBefore == catalogDigestAfter {
		t.Fatalf("catalog digest did not change after editing the substitute runbook's bytes: both %q", catalogDigestBefore)
	}

	// B3: the tool's own catalog entry digest must equal the package
	// digest (not an independent per-file digest) both before and after.
	if len(catBefore.Packages) != 1 || catBefore.Packages[0].Digest != digestBefore {
		t.Fatalf("expected tier-1 entry digest to equal the package digest (before): entry=%q package=%+v", digestBefore, catBefore.Packages)
	}
	if len(catAfter.Packages) != 1 || catAfter.Packages[0].Digest != digestAfter {
		t.Fatalf("expected tier-1 entry digest to equal the package digest (after): entry=%q package=%+v", digestAfter, catAfter.Packages)
	}
}
