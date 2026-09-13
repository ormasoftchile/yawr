package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type resultsDigestVectors struct {
	SchemaVersion    string                `json:"schema_version"`
	Algorithm        string                `json:"algorithm"`
	Canonicalization string                `json:"canonicalization"`
	GeneratedBy      string                `json:"generated_by"`
	Cases            []resultsDigestVector `json:"cases"`
}

type resultsDigestVector struct {
	Name                      string          `json:"name"`
	SemanticRecord            json.RawMessage `json:"semantic_record"`
	SourceUTF8                string          `json:"source_utf8"`
	SourceBase64              string          `json:"source_base64"`
	CanonicalUTF8             string          `json:"canonical_utf8"`
	CanonicalBase64           string          `json:"canonical_base64"`
	SHA256Hex                 string          `json:"sha256_hex"`
	Digest                    string          `json:"digest"`
	CanonicalWithDigestUTF8   string          `json:"canonical_with_digest_utf8"`
	CanonicalWithDigestBase64 string          `json:"canonical_with_digest_base64"`
	Metadata                  struct {
		Description      string   `json:"description"`
		SourceOrder      string   `json:"source_order"`
		EquivalenceGroup string   `json:"equivalence_group"`
		DistinctFrom     []string `json:"distinct_from"`
	} `json:"metadata"`
}

func loadResultsDigestVectors(t *testing.T) resultsDigestVectors {
	t.Helper()
	data, err := os.ReadFile("../../specs/run-results-digest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors resultsDigestVectors
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

func decodeVectorJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRunResultsDigestVectorsIndependentStdlibVerification(t *testing.T) {
	vectors := loadResultsDigestVectors(t)
	expectedNames := []string{
		"root-false", "root-zero", "root-empty-string", "root-array",
		"root-object-order-a", "root-object-order-b", "root-output-absent",
		"root-output-present-null", "root-nested-null", "parent-results", "child-results",
	}
	if vectors.SchemaVersion != "yawr.run-results-digest-v1/v1" || vectors.Algorithm != "sha256" || len(vectors.Cases) != len(expectedNames) {
		t.Fatal("invalid digest vector collection identity")
	}
	byName := make(map[string]resultsDigestVector, len(vectors.Cases))
	for index, vector := range vectors.Cases {
		if vector.Name != expectedNames[index] {
			t.Fatalf("case %d is %q, want %q", index, vector.Name, expectedNames[index])
		}
		source := []byte(vector.SourceUTF8)
		canonical := []byte(vector.CanonicalUTF8)
		withDigest := []byte(vector.CanonicalWithDigestUTF8)
		for encoded, raw := range map[string][]byte{
			vector.SourceBase64: source, vector.CanonicalBase64: canonical, vector.CanonicalWithDigestBase64: withDigest,
		} {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || !bytes.Equal(decoded, raw) {
				t.Fatalf("%s has invalid fixed base64", vector.Name)
			}
		}
		semantic := decodeVectorJSON(t, vector.SemanticRecord)
		sourceValue := decodeVectorJSON(t, source)
		canonicalValue := decodeVectorJSON(t, canonical)
		if !reflect.DeepEqual(sourceValue, semantic) || !reflect.DeepEqual(canonicalValue, semantic) {
			t.Fatalf("%s source/canonical JSON changes semantics", vector.Name)
		}
		stdlibCanonical, err := json.Marshal(semantic)
		if err != nil || !bytes.Equal(stdlibCanonical, canonical) {
			t.Fatalf("%s canonical bytes differ from independent stdlib encoding", vector.Name)
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != vector.SHA256Hex || vector.Digest != "sha256:"+vector.SHA256Hex {
			t.Fatalf("%s fixed digest mismatch", vector.Name)
		}
		withDigestValue := decodeVectorJSON(t, withDigest).(map[string]any)
		if withDigestValue["digest"] != vector.Digest {
			t.Fatalf("%s canonical-with-digest identity mismatch", vector.Name)
		}
		delete(withDigestValue, "digest")
		if !reflect.DeepEqual(withDigestValue, semantic) {
			t.Fatalf("%s canonical-with-digest changes semantics", vector.Name)
		}
		byName[vector.Name] = vector
	}
	a, b := byName["root-object-order-a"], byName["root-object-order-b"]
	if a.CanonicalUTF8 != b.CanonicalUTF8 || a.Digest != b.Digest || a.Metadata.EquivalenceGroup != "root-object-order" || b.Metadata.EquivalenceGroup != "root-object-order" {
		t.Fatal("object ordering equivalence vector is not equivalent")
	}
	for _, vector := range vectors.Cases {
		for _, otherName := range vector.Metadata.DistinctFrom {
			other, ok := byName[otherName]
			if !ok || vector.CanonicalUTF8 == other.CanonicalUTF8 || vector.Digest == other.Digest {
				t.Fatalf("%s distinction from %s is not fixed", vector.Name, otherName)
			}
		}
	}
}

func TestRunResultsDigestVectorsImplementationParity(t *testing.T) {
	for _, vector := range loadResultsDigestVectors(t).Cases {
		t.Run(vector.Name, func(t *testing.T) {
			var record RunResults
			decoder := json.NewDecoder(strings.NewReader(vector.SourceUTF8))
			decoder.UseNumber()
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			withoutDigest, err := CanonicalResultsJSON(&record, false)
			if err != nil || string(withoutDigest) != vector.CanonicalUTF8 {
				t.Fatalf("canonical parity mismatch: %s (%v)", withoutDigest, err)
			}
			if err := record.Seal(); err != nil {
				t.Fatal(err)
			}
			if record.Digest != vector.Digest {
				t.Fatalf("seal parity mismatch: %s", record.Digest)
			}
			if err := record.Validate(); err != nil {
				t.Fatal(err)
			}
			withDigest, err := CanonicalResultsJSON(&record, true)
			if err != nil || string(withDigest) != vector.CanonicalWithDigestUTF8 {
				t.Fatalf("canonical-with-digest parity mismatch: %s (%v)", withDigest, err)
			}
		})
	}
}
