package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	if err := yaml.Unmarshal(data, &manifest); err != nil {
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
	yawrMin := strings.TrimSpace(manifest.Compatibility.YawrMinVersion)
	if yawrMin == "" {
		return nil, fmt.Errorf("extension manifest: compatibility.yawr_min_version is required")
	}
	yawrMax := strings.TrimSpace(manifest.Compatibility.YawrMaxVersion)
	if !semverRe.MatchString(yawrMin) {
		return nil, fmt.Errorf("extension manifest: invalid minimum version %q", yawrMin)
	}
	if yawrMax != "" && !semverRe.MatchString(yawrMax) {
		return nil, fmt.Errorf("extension manifest: invalid maximum version %q", yawrMax)
	}
	manifest.Compatibility.YawrMinVersion = yawrMin
	manifest.Compatibility.YawrMaxVersion = yawrMax
	for _, cap := range manifest.Capabilities {
		if _, ok := knownCapabilities[cap]; !ok {
			return nil, fmt.Errorf("extension manifest: unknown capability %q", cap)
		}
	}
	if err := validateCompatibility(manifest.Compatibility); err != nil {
		return nil, err
	}
	return manifest, nil
}

type semver struct {
	major int
	minor int
	patch int
}

func validateCompatibility(comp extension.Compatibility) error {
	host, err := parseSemver(hostVersion)
	if err != nil {
		return err
	}
	min, err := parseSemver(comp.YawrMinVersion)
	if err != nil {
		return err
	}
	if compareSemver(host, min) < 0 {
		return fmt.Errorf("extension manifest: host version %s below minimum %s", hostVersion, comp.YawrMinVersion)
	}
	if comp.YawrMaxVersion != "" {
		max, err := parseSemver(comp.YawrMaxVersion)
		if err != nil {
			return err
		}
		if compareSemver(host, max) > 0 {
			return fmt.Errorf("extension manifest: host version %s above maximum %s", hostVersion, comp.YawrMaxVersion)
		}
	}
	return nil
}

func parseSemver(raw string) (semver, error) {
	clean := strings.SplitN(raw, "-", 2)[0]
	clean = strings.SplitN(clean, "+", 2)[0]
	parts := strings.Split(clean, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q", raw)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, err
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, err
	}
	return semver{major: major, minor: minor, patch: patch}, nil
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	return 0
}
