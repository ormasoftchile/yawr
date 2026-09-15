package toolscope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func validDigest(value string, prefixed bool) bool {
	if prefixed {
		if !strings.HasPrefix(value, "sha256:") {
			return false
		}
		value = strings.TrimPrefix(value, "sha256:")
	}
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateTarget(target Target, document Document, contextDigest string) error {
	if target.SchemaVersion != TargetVersion || target.RunbookID == "" || target.RunbookName == "" ||
		target.AbsPath == "" || target.AbsPath != document.SourceIdentity ||
		target.PackageName == "" || target.PackageVersion == "" ||
		!validDigest(target.ContentHash, false) || !validDigest(target.SourceDigest, true) ||
		!validDigest(target.PackageDigest, true) {
		return fmt.Errorf("unsupported target version or incomplete captured identity")
	}
	closure, err := schema.DecodeScopedFlowClosure(target.ExecutableClosure)
	if err != nil {
		return err
	}
	if closure.RootScopeID != target.ScopeID || closure.ScopeContextDigest != contextDigest ||
		closure.ClosureDigest != target.ClosureDigest || closure.Invocation == nil {
		return fmt.Errorf("captured closure ownership, digest or invocation mismatch")
	}
	actual, err := json.Marshal(closure.Invocation)
	if err != nil {
		return err
	}
	expected, err := json.Marshal(&schema.RunbookInvocation{
		Bindings: target.Bindings, Outputs: target.Outputs, Results: closure.Invocation.Results,
	})
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("captured invocation declarations differ from target metadata")
	}
	if err := schema.ValidateTypedRunbook(&schema.Runbook{Inputs: target.Inputs, Bindings: target.Bindings, Outputs: target.Outputs}); err != nil {
		return fmt.Errorf("invalid captured declarations: %w", err)
	}
	return nil
}
