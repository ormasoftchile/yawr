package pkgcatalog

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

func ReadProjectBindings(source *Source, workspace, packageMap string) (*schema.ProjectConfig, []BindingOrigin, error) {
	read := func(path string, optional bool) (*schema.ProjectConfig, error) {
		data, err := source.read(path)
		if optional && os.IsNotExist(err) {
			return &schema.ProjectConfig{APIVersion: schema.ProjectConfigAPIVersion}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var config schema.ProjectConfig
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if config.APIVersion != schema.ProjectConfigAPIVersion {
			return nil, fmt.Errorf("%s: apiVersion must be %q", path, schema.ProjectConfigAPIVersion)
		}
		return &config, nil
	}
	project, err := read(filepath.Join(workspace, ".yawr", "config.yaml"), true)
	if err != nil {
		return nil, nil, err
	}
	var override schema.ProjectConfig
	if packageMap != "" {
		if !filepath.IsAbs(packageMap) {
			packageMap = filepath.Join(workspace, packageMap)
		}
		loaded, err := read(packageMap, false)
		if err != nil {
			return nil, nil, err
		}
		override = *loaded
	}
	requirements, origins := MergePackageBindings(project.Requires, override.Requires)
	return &schema.ProjectConfig{APIVersion: schema.ProjectConfigAPIVersion,
		Requires: requirements, ToolPaths: append(append([]string(nil), project.ToolPaths...), override.ToolPaths...)}, origins, nil
}
