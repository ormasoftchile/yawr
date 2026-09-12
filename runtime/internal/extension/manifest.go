package extension

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"gopkg.in/yaml.v3"
)

var semverRe = regexp.MustCompile("^\\d+\\.\\d+\\.\\d+(?:-[0-9A-Za-z.-]+)?(?:\\+[0-9A-Za-z.-]+)?$")

var knownCapabilities = map[string]struct{}{
	extension.CapabilityToolRegistration:     {},
	extension.CapabilityPolicyContribution:   {},
	extension.CapabilityProviderRegistration: {},
	extension.CapabilityReadEnv:              {},
	extension.CapabilityReadFiles:            {},
	extension.CapabilityExecProcess:          {},
}

const manifestFilename = "yawr-extension.yaml"

// readManifest parses one selected extension manifest file.
func readManifest(path string) (*extension.ExtensionManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return readManifestBytes(path, data)
}

func readManifestDir(path string) (*extension.ExtensionManifest, string, error) {
	manifestPath := filepath.Join(path, manifestFilename)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", err
	}
	manifest, err := readManifestBytes(manifestPath, data)
	return manifest, manifestPath, err
}

func readManifestBytes(path string, data []byte) (*extension.ExtensionManifest, error) {
	var manifest extension.ExtensionManifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return validateManifest(&manifest)
}

func validateManifest(manifest *extension.ExtensionManifest) (*extension.ExtensionManifest, error) {
	if strings.TrimSpace(manifest.Name) == "" {
		return nil, fmt.Errorf("extension manifest: name is required")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return nil, fmt.Errorf("extension manifest: version is required")
	}
	if !semverRe.MatchString(manifest.Version) {
		return nil, fmt.Errorf("extension manifest: invalid version %q", manifest.Version)
	}
	if strings.TrimSpace(manifest.Entrypoint) == "" {
		return nil, fmt.Errorf("extension manifest: entrypoint is required")
	}
	for _, cap := range manifest.Capabilities {
		if _, ok := knownCapabilities[cap]; !ok {
			return nil, fmt.Errorf("extension manifest: unknown capability %q", cap)
		}
	}
	return manifest, nil
}
