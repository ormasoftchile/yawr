package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanSchemaDirWithoutTestsExcludesTestToolFixtures(t *testing.T) {
	root := t.TempDir()
	productionPath := filepath.Join(root, "packages", "incident-routing", "tools", "icm.tool.yaml")
	testPath := filepath.Join(root, "tests", "packages", "incident-routing-mock", "tools", "icm.tool.yaml")

	writeTool := func(path, action string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "apiVersion: yawr.tool/v1\n" +
			"meta:\n  name: icm\n  version: 1.0.0\n" +
			"transport:\n  mode: native\n  command: does-not-run\n" +
			"actions:\n  - name: " + action + "\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeTool(productionPath, "get-incident")
	writeTool(testPath, "test-only-action")

	defs, err := ScanSchemaDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanSchemaDirWithoutTests: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("scanned %d definitions, want only the production definition", len(defs))
	}
	if _, ok := defs[0].Actions["get-incident"]; !ok {
		t.Fatalf("production get-incident action was not retained: %v", defs[0].Actions)
	}
	if _, ok := defs[0].Actions["test-only-action"]; ok {
		t.Fatal("test-only action must not be present in the serving registry")
	}
}

func TestScanSchemaDirAcceptsYawtFiles(t *testing.T) {
	root := t.TempDir()
	toolYamlPath := filepath.Join(root, "packages", "tools", "classic.tool.yaml")
	yawtPath := filepath.Join(root, "packages", "tools", "modern.yawt")

	writeTool := func(path, name, action string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "apiVersion: yawr.tool/v1\n" +
			"meta:\n  name: " + name + "\n  version: 1.0.0\n" +
			"transport:\n  mode: native\n  command: echo\n" +
			"actions:\n  - name: " + action + "\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeTool(toolYamlPath, "classic", "act")
	writeTool(yawtPath, "modern", "act")

	defs, err := ScanSchemaDirWithoutTests(root)
	if err != nil {
		t.Fatalf("ScanSchemaDirWithoutTests: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("scanned %d definitions, want 2 (both .tool.yaml and .yawt)", len(defs))
	}
}
