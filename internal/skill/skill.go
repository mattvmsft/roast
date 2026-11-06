// Package skill installs and inspects Roast's embedded agent skill.
package skill

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const assetRoot = "assets/roast"

//go:embed all:assets/roast
var assets embed.FS

// State describes the installed skill's relationship to this binary.
type State string

const (
	NotInstalled State = "not_installed"
	Symlink      State = "symlink"
	UpToDate     State = "up_to_date"
	Stale        State = "stale"
)

// Status is the result of inspecting one skill installation.
type Status struct {
	State State
	Path  string
	Stale []string
}

// InstallationTarget is one harness-visible parent directory for the skill.
type InstallationTarget struct {
	Harness string
	Parent  string
	Path    string
}

// DefaultParent returns the shared user skill directory used by Codex and
// other agents which implement the Agent Skills convention.
func DefaultParent() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

// Target returns the roast skill directory below parent. An empty parent uses
// DefaultParent.
func Target(parent string) (string, error) {
	if parent == "" {
		var err error
		parent, err = DefaultParent()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(parent, "roast"), nil
}

// InstallationTargets returns the explicit --dir target, or every detected
// harness skill location. The shared Agent Skills location is always included
// because it is the portable target used by Codex and other compatible agents.
func InstallationTargets(parent string) ([]InstallationTarget, error) {
	if parent != "" {
		path, err := Target(parent)
		if err != nil {
			return nil, err
		}
		return []InstallationTarget{{Harness: "custom", Parent: parent, Path: path}}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user home: %w", err)
	}
	claudeRoot := configuredRoot(home, os.Getenv("CLAUDE_CONFIG_DIR"), ".claude")
	claudePresent := os.Getenv("CLAUDE_CONFIG_DIR") != "" || directoryExists(claudeRoot)
	if !claudePresent {
		_, claudeErr := exec.LookPath("claude")
		claudePresent = claudeErr == nil
	}
	configRoot := configuredRoot(home, os.Getenv("XDG_CONFIG_HOME"), ".config")
	polytokenRoot := filepath.Join(configRoot, "polytoken")
	polytokenPresent := directoryExists(polytokenRoot)
	if !polytokenPresent {
		_, polytokenErr := exec.LookPath("polytoken")
		polytokenPresent = polytokenErr == nil
	}
	return installationTargets(home, claudeRoot, claudePresent, polytokenRoot, polytokenPresent), nil
}

func installationTargets(home, claudeRoot string, claudePresent bool, polytokenRoot string, polytokenPresent bool) []InstallationTarget {
	parents := []InstallationTarget{{Harness: "agents", Parent: filepath.Join(home, ".agents", "skills")}}
	if claudePresent {
		parents = append(parents, InstallationTarget{Harness: "claude", Parent: filepath.Join(claudeRoot, "skills")})
	}
	if polytokenPresent {
		parents = append(parents, InstallationTarget{Harness: "polytoken", Parent: filepath.Join(polytokenRoot, "skills")})
	}
	seen := make(map[string]bool, len(parents))
	out := make([]InstallationTarget, 0, len(parents))
	for _, target := range parents {
		target.Parent = filepath.Clean(target.Parent)
		target.Path = filepath.Join(target.Parent, "roast")
		if seen[target.Path] {
			continue
		}
		seen[target.Path] = true
		out = append(out, target)
	}
	return out
}

func configuredRoot(home, override, fallback string) string {
	switch {
	case override == "":
		return filepath.Join(home, fallback)
	case override == "~":
		return home
	case strings.HasPrefix(override, "~/"):
		return filepath.Join(home, override[2:])
	default:
		return filepath.Clean(override)
	}
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func embeddedFiles() (map[string][]byte, error) {
	files := make(map[string][]byte)
	err := fs.WalkDir(assets, assetRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(assetRoot, path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	return files, err
}

// Inspect reports whether target contains the skill embedded in this binary.
func Inspect(target string) (Status, error) {
	status := Status{State: NotInstalled, Path: target}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("stat skill directory %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		status.State = Symlink
		return status, nil
	}
	if !info.IsDir() {
		return status, fmt.Errorf("skill path %s is not a directory", target)
	}

	files, err := embeddedFiles()
	if err != nil {
		return status, fmt.Errorf("read embedded skill: %w", err)
	}
	for rel, want := range files {
		got, readErr := os.ReadFile(filepath.Join(target, rel))
		if readErr != nil || !bytes.Equal(got, want) {
			status.Stale = append(status.Stale, rel)
		}
	}
	sort.Strings(status.Stale)
	if len(status.Stale) > 0 {
		status.State = Stale
	} else {
		status.State = UpToDate
	}
	return status, nil
}

// Install writes the embedded skill into target. Existing differing files are
// preserved unless force is true. A symlinked target is treated as a developer
// checkout and is never modified.
func Install(target string, force bool) (Status, error) {
	status, err := Inspect(target)
	if err != nil {
		return status, err
	}
	switch status.State {
	case Symlink, UpToDate:
		return status, nil
	case Stale:
		if !force {
			return status, fmt.Errorf("skill files differ; re-run with --force to overwrite")
		}
	}

	files, err := embeddedFiles()
	if err != nil {
		return status, fmt.Errorf("read embedded skill: %w", err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return status, fmt.Errorf("create skill directory %s: %w", target, err)
	}
	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		destination := filepath.Join(target, rel)
		if info, statErr := os.Lstat(destination); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return status, fmt.Errorf("refusing to overwrite symlink %s", destination)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return status, fmt.Errorf("stat %s: %w", destination, statErr)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return status, fmt.Errorf("create skill directory: %w", err)
		}
		if err := os.WriteFile(destination, files[rel], 0o644); err != nil {
			return status, fmt.Errorf("write skill file %s: %w", destination, err)
		}
	}
	return Inspect(target)
}
