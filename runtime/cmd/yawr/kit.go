package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	catalogURL      = "https://raw.githubusercontent.com/ormasoftchile/yawr-catalog/main/catalog.yaml"
	kitfilePath     = "Kitfile.yaml"
	kitfileLockPath = "Kitfile.lock"
	kitsDir         = ".kits"
	defaultVersion  = ">=1.0.0"
	kitAPIVersion   = "yawr.kit/v1"
	catalogAPIVer   = "yawr.catalog/v1"
)

type catalogYAML struct {
	APIVersion string     `yaml:"apiVersion"`
	Kits       []kitEntry `yaml:"kits"`
}

type kitEntry struct {
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Repo        string       `yaml:"repo"`
	Versions    []kitVersion `yaml:"versions"`
}

type kitVersion struct {
	Version    string `yaml:"version"`
	Ref        string `yaml:"ref"`
	ReleasedAt string `yaml:"released_at"`
}

type kitfile struct {
	APIVersion string       `yaml:"apiVersion"`
	Kits       []kitfileKit `yaml:"kits"`
}

type kitfileKit struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

type lockfile struct {
	APIVersion string        `yaml:"apiVersion"`
	ResolvedAt string        `yaml:"resolved_at"`
	Kits       []resolvedKit `yaml:"kits"`
}

type resolvedKit struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Ref     string `yaml:"ref"`
	Repo    string `yaml:"repo"`
	Path    string `yaml:"path"`
}

func runKit(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: yawr kit <subcommand>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  install <kit>   Add kit and fetch it (creates Kitfile.yaml if needed)")
		fmt.Fprintln(os.Stderr, "  search [query]  Search kits in catalog")
		fmt.Fprintln(os.Stderr, "  add <kit-name>  Add kit to Kitfile.yaml")
		fmt.Fprintln(os.Stderr, "  fetch           Fetch all kits in Kitfile.yaml")
		fmt.Fprintln(os.Stderr, "  list            List installed kits")
		return exitValidation
	}

	switch args[0] {
	case "install":
		return runKitInstall(args[1:])
	case "search":
		return runKitSearch(args[1:])
	case "add":
		return runKitAdd(args[1:])
	case "fetch":
		return runKitFetch(args[1:])
	case "list":
		return runKitList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown kit subcommand: %s\n", args[0])
		return exitValidation
	}
}

func runKitSearch(args []string) int {
	fs := flag.NewFlagSet("kit search", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	query := ""
	if fs.NArg() > 0 {
		query = fs.Arg(0)
	}

	catalog, err := fetchCatalog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit search: %v\n", err)
		return exitRuntime
	}

	matches := filterKits(catalog.Kits, query)
	if len(matches) == 0 {
		fmt.Fprintln(os.Stdout, "no kits found")
		return exitSuccess
	}

	fmt.Fprintf(os.Stdout, "%-30s  %-12s  %s\n", "NAME", "VERSION", "DESCRIPTION")
	fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))
	for _, kit := range matches {
		latestVersion := getLatestVersion(kit.Versions)
		fmt.Fprintf(os.Stdout, "%-30s  %-12s  %s\n", kit.Name, latestVersion, kit.Description)
	}
	return exitSuccess
}

func runKitAdd(args []string) int {
	fs := flag.NewFlagSet("kit add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kit add: kit name required")
		return exitValidation
	}
	kitName := fs.Arg(0)

	catalog, err := fetchCatalog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit add: %v\n", err)
		return exitRuntime
	}

	if !kitExistsInCatalog(catalog, kitName) {
		fmt.Fprintf(os.Stderr, "kit add: kit %q not found in catalog\n", kitName)
		return exitValidation
	}

	kitfile, err := loadOrCreateKitfile(kitfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit add: %v\n", err)
		return exitRuntime
	}

	if kitInKitfile(kitfile, kitName) {
		fmt.Fprintf(os.Stderr, "kit add: kit %q already in Kitfile.yaml\n", kitName)
		return exitValidation
	}

	kitfile.Kits = append(kitfile.Kits, kitfileKit{
		Name:    kitName,
		Version: defaultVersion,
	})

	if err := saveKitfile(kitfile, kitfilePath); err != nil {
		fmt.Fprintf(os.Stderr, "kit add: %v\n", err)
		return exitRuntime
	}

	fmt.Fprintf(os.Stdout, "kit add: added %q to Kitfile.yaml\n", kitName)
	return exitSuccess
}

func runKitInstall(args []string) int {
	fs := flag.NewFlagSet("kit install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kit install: kit name required")
		fmt.Fprintln(os.Stderr, "usage: yawr kit install <kit-name>")
		return exitValidation
	}

	catalog, err := fetchCatalog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit install: %v\n", err)
		return exitRuntime
	}

	kf, err := loadOrCreateKitfile(kitfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit install: %v\n", err)
		return exitRuntime
	}

	// Add each requested kit to the manifest.
	for _, kitName := range fs.Args() {
		if !kitExistsInCatalog(catalog, kitName) {
			fmt.Fprintf(os.Stderr, "kit install: kit %q not found in catalog\n", kitName)
			return exitValidation
		}
		if !kitInKitfile(kf, kitName) {
			kf.Kits = append(kf.Kits, kitfileKit{Name: kitName, Version: defaultVersion})
		}
	}

	if err := saveKitfile(kf, kitfilePath); err != nil {
		fmt.Fprintf(os.Stderr, "kit install: write Kitfile.yaml: %v\n", err)
		return exitRuntime
	}

	// Now fetch everything in the manifest.
	if err := os.MkdirAll(kitsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "kit install: create .kits dir: %v\n", err)
		return exitRuntime
	}

	lock := lockfile{
		APIVersion: kitAPIVersion,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		Kits:       []resolvedKit{},
	}

	for _, k := range kf.Kits {
		resolved, err := resolveKit(catalog, k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kit install: resolve %s: %v\n", k.Name, err)
			return exitRuntime
		}
		destPath := filepath.Join(kitsDir, k.Name)
		if err := downloadKit(resolved.Repo, resolved.Ref, destPath); err != nil {
			fmt.Fprintf(os.Stderr, "kit install: download %s: %v\n", k.Name, err)
			return exitRuntime
		}
		resolved.Path = destPath
		lock.Kits = append(lock.Kits, resolved)
		fmt.Fprintf(os.Stdout, "kit install: installed %s@%s\n", k.Name, resolved.Version)
	}

	if err := saveLockfile(&lock, kitfileLockPath); err != nil {
		fmt.Fprintf(os.Stderr, "kit install: write lockfile: %v\n", err)
		return exitRuntime
	}

	fmt.Fprintf(os.Stdout, "kit install: done (%d kits)\n", len(lock.Kits))
	return exitSuccess
}

func runKitFetch(args []string) int {
	fs := flag.NewFlagSet("kit fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	kitfile, err := loadKitfile(kitfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit fetch: %v\n", err)
		return exitRuntime
	}

	catalog, err := fetchCatalog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kit fetch: %v\n", err)
		return exitRuntime
	}

	if err := os.MkdirAll(kitsDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "kit fetch: create .kits dir: %v\n", err)
		return exitRuntime
	}

	lock := lockfile{
		APIVersion: kitAPIVersion,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		Kits:       []resolvedKit{},
	}

	for _, k := range kitfile.Kits {
		resolved, err := resolveKit(catalog, k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kit fetch: resolve %s: %v\n", k.Name, err)
			return exitRuntime
		}

		destPath := filepath.Join(kitsDir, k.Name)
		if err := downloadKit(resolved.Repo, resolved.Ref, destPath); err != nil {
			fmt.Fprintf(os.Stderr, "kit fetch: download %s: %v\n", k.Name, err)
			return exitRuntime
		}

		resolved.Path = destPath
		lock.Kits = append(lock.Kits, resolved)
		fmt.Fprintf(os.Stdout, "kit fetch: downloaded %s@%s\n", k.Name, resolved.Version)
	}

	if err := saveLockfile(&lock, kitfileLockPath); err != nil {
		fmt.Fprintf(os.Stderr, "kit fetch: write lockfile: %v\n", err)
		return exitRuntime
	}

	fmt.Fprintf(os.Stdout, "kit fetch: complete (%d kits)\n", len(lock.Kits))
	return exitSuccess
}

func runKitList(args []string) int {
	fs := flag.NewFlagSet("kit list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	lock, lockErr := loadLockfile(kitfileLockPath)
	if lockErr == nil {
		if len(lock.Kits) == 0 {
			fmt.Fprintln(os.Stdout, "no kits installed")
			return exitSuccess
		}
		fmt.Fprintf(os.Stdout, "%-30s  %-12s  %s\n", "NAME", "VERSION", "PATH")
		fmt.Fprintln(os.Stdout, strings.Repeat("-", 80))
		for _, k := range lock.Kits {
			fmt.Fprintf(os.Stdout, "%-30s  %-12s  %s\n", k.Name, k.Version, k.Path)
		}
		return exitSuccess
	}

	kitfile, kitErr := loadKitfile(kitfilePath)
	if kitErr != nil {
		fmt.Fprintln(os.Stderr, "kit list: no Kitfile.yaml or Kitfile.lock found")
		return exitRuntime
	}

	if len(kitfile.Kits) == 0 {
		fmt.Fprintln(os.Stdout, "no kits in Kitfile.yaml")
		return exitSuccess
	}

	fmt.Fprintf(os.Stdout, "%-30s  %s\n", "NAME", "VERSION")
	fmt.Fprintln(os.Stdout, strings.Repeat("-", 50))
	for _, k := range kitfile.Kits {
		fmt.Fprintf(os.Stdout, "%-30s  %s\n", k.Name, k.Version)
	}
	return exitSuccess
}

func fetchCatalog() (*catalogYAML, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", catalogURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch catalog: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var catalog catalogYAML
	if err := yaml.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}

	return &catalog, nil
}

func filterKits(kits []kitEntry, query string) []kitEntry {
	if query == "" {
		return kits
	}
	query = strings.ToLower(query)
	var matches []kitEntry
	for _, kit := range kits {
		if strings.Contains(strings.ToLower(kit.Name), query) ||
			strings.Contains(strings.ToLower(kit.Description), query) {
			matches = append(matches, kit)
		}
	}
	return matches
}

func getLatestVersion(versions []kitVersion) string {
	if len(versions) == 0 {
		return "unknown"
	}
	return versions[len(versions)-1].Version
}

func kitExistsInCatalog(catalog *catalogYAML, name string) bool {
	for _, kit := range catalog.Kits {
		if kit.Name == name {
			return true
		}
	}
	return false
}

func loadOrCreateKitfile(path string) (*kitfile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &kitfile{
			APIVersion: kitAPIVersion,
			Kits:       []kitfileKit{},
		}, nil
	}
	return loadKitfile(path)
}

func loadKitfile(path string) (*kitfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf kitfile
	if err := yaml.Unmarshal(data, &kf); err != nil {
		return nil, err
	}
	return &kf, nil
}

func loadLockfile(path string) (*lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf lockfile
	if err := yaml.Unmarshal(data, &lf); err != nil {
		return nil, err
	}
	return &lf, nil
}

func kitInKitfile(kf *kitfile, name string) bool {
	for _, k := range kf.Kits {
		if k.Name == name {
			return true
		}
	}
	return false
}

func saveKitfile(kf *kitfile, path string) error {
	data, err := yaml.Marshal(kf)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func saveLockfile(lf *lockfile, path string) error {
	data, err := yaml.Marshal(lf)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func resolveKit(catalog *catalogYAML, k kitfileKit) (resolvedKit, error) {
	for _, entry := range catalog.Kits {
		if entry.Name != k.Name {
			continue
		}
		if len(entry.Versions) == 0 {
			return resolvedKit{}, fmt.Errorf("no versions for kit %s", k.Name)
		}
		latest := entry.Versions[len(entry.Versions)-1]
		return resolvedKit{
			Name:    k.Name,
			Version: latest.Version,
			Ref:     latest.Ref,
			Repo:    entry.Repo,
		}, nil
	}
	return resolvedKit{}, fmt.Errorf("kit %s not found in catalog", k.Name)
}

func downloadKit(repo, ref, destPath string) error {
	if err := os.RemoveAll(destPath); err != nil {
		return fmt.Errorf("remove old kit: %w", err)
	}

	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", ref, repo, destPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("git", "clone", repo, destPath)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
		cmd = exec.Command("git", "checkout", ref)
		cmd.Dir = destPath
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git checkout: %w", err)
		}
	}
	return nil
}
