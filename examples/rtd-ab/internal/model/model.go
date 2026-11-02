// SPDX-License-Identifier: Apache-2.0

// Package model checks for the presence of a delivered model at MODEL_PATH and
// reports what it found. This container is a no-op and does not use model data;
// the check exists so onboarding can confirm the model-delivery step wired up.
//
// It follows the structure in the Index Cloud KB "Updating models in containers
// without rebuilding": a versioned folder containing a manifest.json (a
// {"name","models":[...]} list) plus a model_data/ directory holding the files
// the manifest lists. Models are optional, so absence is reported, not treated
// as an error.
package model

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Info is the result of a presence check.
type Info struct {
	Present bool   // a manifest was found and every file it lists exists
	Version string // CalVer folder name, only when MODEL_PATH is a parent of
	// versioned folders; empty when the manifest is at MODEL_PATH
	// itself (the runtime case: Index mounts the versioned
	// folder's contents there, so the version is not on the path)
	Dir     string   // directory the model resolved to
	Name    string   // manifest "name" field
	Files   []string // manifest-listed files that exist on disk
	Missing []string // manifest-listed files that were not found
	Err     error    // read/parse problem (non-fatal to the caller)
}

// Label is a human/metric identifier for the model: the CalVer folder version if
// one was on the path, else the manifest name, else "present"; "none" if absent.
func (i Info) Label() string {
	switch {
	case !i.Present && i.Version == "" && i.Name == "":
		return "none"
	case i.Version != "":
		return i.Version
	case i.Name != "":
		return i.Name
	default:
		return "present"
	}
}

type manifest struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// Check resolves the model at path and validates it against its manifest. path
// may directly contain manifest.json, or be a parent of CalVer version folders
// (highest version wins), matching how Index mounts model artifacts at MODEL_PATH.
func Check(path string) Info {
	dir, version, err := resolveDir(path)
	if err != nil {
		return Info{Err: err}
	}
	if dir == "" {
		return Info{} // no manifest found; absent (Label() reports "none")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Info{Version: version, Dir: dir, Err: fmt.Errorf("read manifest: %w", err)}
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Info{Version: version, Dir: dir, Err: fmt.Errorf("parse manifest: %w", err)}
	}
	info := Info{Version: version, Dir: dir, Name: m.Name}
	for _, f := range m.Models {
		if locate(dir, f) != "" {
			info.Files = append(info.Files, f)
		} else {
			info.Missing = append(info.Missing, f)
		}
	}
	info.Present = len(m.Models) > 0 && len(info.Missing) == 0
	return info
}

// resolveDir returns the directory holding manifest.json and its version label.
func resolveDir(root string) (dir, version string, err error) {
	fi, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", err
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("MODEL_PATH %q is not a directory", root)
	}
	if fileExists(filepath.Join(root, "manifest.json")) {
		// Manifest at MODEL_PATH itself: this is what Index mounts (the versioned
		// folder's contents land here), so there is no version folder on the path.
		return root, "", nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", "", err
	}
	var versions []string
	for _, e := range ents {
		if e.IsDir() && fileExists(filepath.Join(root, e.Name(), "manifest.json")) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", "", nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))
	return filepath.Join(root, versions[0]), versions[0], nil
}

// locate finds a manifest-listed file at the folder root or under model_data/.
func locate(dir, name string) string {
	for _, p := range []string{filepath.Join(dir, name), filepath.Join(dir, "model_data", name)} {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
