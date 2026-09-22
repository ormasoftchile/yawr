package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

func runCompile(args []string) int {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	target := fs.String("target", "", "Target platform: ios, android, or mobile (validates both)")
	outputDir := fs.String("output", ".", "Output directory for compiled artifacts")
	kitName := fs.String("kit-name", "", "Platform kit name (required)")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitValidation
	}

	if *target == "" {
		fmt.Fprintln(os.Stderr, "compile: --target is required")
		return exitValidation
	}

	if *kitName == "" {
		fmt.Fprintln(os.Stderr, "compile: --kit-name is required")
		return exitValidation
	}

	var targetPlatforms []string
	switch *target {
	case "ios":
		targetPlatforms = []string{"ios"}
	case "android":
		targetPlatforms = []string{"android"}
	case "mobile":
		targetPlatforms = []string{"ios", "android"}
	default:
		fmt.Fprintf(os.Stderr, "compile: invalid --target: %s (must be ios, android, or mobile)\n", *target)
		return exitValidation
	}

	// Find all .tool.yaml files in current directory and subdirectories
	toolFiles, err := findToolFiles(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: find tool files: %v\n", err)
		return exitFailure
	}

	if len(toolFiles) == 0 {
		fmt.Fprintln(os.Stderr, "compile: no .tool.yaml files found")
		return exitValidation
	}

	// Load and validate all tools
	var tools []*schema.ToolDef
	for _, path := range toolFiles {
		tool, err := loadToolDef(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "compile: %s: %v\n", path, err)
			return exitValidation
		}
		tools = append(tools, tool)
	}

	// Validate platform compatibility
	for _, platform := range targetPlatforms {
		if err := validatePlatform(tools, platform); err != nil {
			fmt.Fprintf(os.Stderr, "compile: platform %s: %v\n", platform, err)
			return exitValidation
		}
	}

	// Create output directory if needed
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "compile: create output dir: %v\n", err)
		return exitFailure
	}

	// Write manifest.json
	manifest := map[string]string{
		"name":        *kitName,
		"target":      *target,
		"compiled-at": time.Now().UTC().Format(time.RFC3339),
	}
	manifestPath := filepath.Join(*outputDir, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: marshal manifest: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "compile: write manifest: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(os.Stderr, "✓ Compiled %d tools for %s\n", len(tools), *target)
	fmt.Fprintf(os.Stderr, "✓ Manifest written to %s\n", manifestPath)
	return exitSuccess
}

func findToolFiles(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".tool.yaml") || strings.HasSuffix(path, ".yawt")) {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func loadToolDef(path string) (*schema.ToolDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tool schema.ToolDef
	if err := yaml.Unmarshal(data, &tool); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &tool, nil
}

func validatePlatform(tools []*schema.ToolDef, platform string) error {
	for _, tool := range tools {
		// If tool has no impl blocks at all, it's a pure server-side tool (allowed)
		if len(tool.Impl) == 0 {
			continue
		}
		// If tool has impl blocks but is missing this platform, fail
		if _, ok := tool.Impl[platform]; !ok {
			return fmt.Errorf("tool %s: capability unavailable on platform %s (has impl blocks for other platforms but not %s)",
				tool.Name, platform, platform)
		}
	}
	return nil
}
