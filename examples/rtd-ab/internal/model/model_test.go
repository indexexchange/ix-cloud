// SPDX-License-Identifier: Apache-2.0

package model

import (
	"os"
	"path/filepath"
	"testing"
)

// writeModel lays out a versioned folder (manifest.json + model_data/<files>)
// under root and returns nothing; helper for the tests.
func writeModel(t *testing.T, root, version, manifestJSON string, dataFiles ...string) {
	t.Helper()
	dir := filepath.Join(root, version, "model_data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, version, "manifest.json"), []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range dataFiles {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckPresentPicksHighestVersion(t *testing.T) {
	root := t.TempDir()
	writeModel(t, root, "2026.09.20.0", `{"name":"old","models":["a.csv"]}`, "a.csv")
	writeModel(t, root, "2026.09.21.0", `{"name":"new","models":["a.csv","b.csv"]}`, "a.csv", "b.csv")

	got := Check(root)
	if !got.Present {
		t.Fatalf("expected present, got %+v", got)
	}
	if got.Version != "2026.09.21.0" {
		t.Errorf("version: got %q want highest 2026.09.21.0", got.Version)
	}
	if len(got.Files) != 2 || len(got.Missing) != 0 {
		t.Errorf("files=%v missing=%v", got.Files, got.Missing)
	}
}

func TestCheckMissingFileIsNotPresent(t *testing.T) {
	root := t.TempDir()
	writeModel(t, root, "2026.09.21.0", `{"name":"n","models":["a.csv","gone.csv"]}`, "a.csv")
	got := Check(root)
	if got.Present {
		t.Errorf("expected not present when a listed file is missing: %+v", got)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "gone.csv" {
		t.Errorf("missing: %v", got.Missing)
	}
}

func TestCheckAbsentIsNotError(t *testing.T) {
	got := Check(filepath.Join(t.TempDir(), "does-not-exist"))
	if got.Present || got.Err != nil || got.Label() != "none" {
		t.Errorf("absent model should be clean with Label \"none\", got %+v", got)
	}
}

func TestCheckManifestAtRoot(t *testing.T) {
	// The runtime case: Index mounts the versioned folder's contents at MODEL_PATH,
	// so manifest.json sits at the root and there is no version on the path.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "model_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"name":"aug-20","models":["m.csv"]}`), 0o644)
	os.WriteFile(filepath.Join(root, "model_data", "m.csv"), []byte("x"), 0o644)
	got := Check(root)
	if !got.Present {
		t.Errorf("manifest at MODEL_PATH root should resolve: %+v", got)
	}
	if got.Version != "" {
		t.Errorf("no version folder on path, want empty Version, got %q", got.Version)
	}
	if got.Label() != "aug-20" {
		t.Errorf("Label should fall back to manifest name, got %q", got.Label())
	}
}
