// SPDX-License-Identifier: Apache-2.0

// Package abmodel reads the experiment's mix and parameters from the models
// folder (MODEL_PATH), so a new model version changes them without a new image.
//
// Layout, as for every Index model (a single manifest.json at the top of the
// version folder, all content under model_data/; Index mounts the versioned S3
// folder's contents at MODEL_PATH, and internal/model resolves the folder):
//
//	manifest.json                       lists the files below (relative to model_data/)
//	model_data/ab-config.json           which model each arm runs, and the percent mix
//	model_data/model_a/content.json     content-signal settings of that model
//	model_data/model_b/content.json
//
// ab-config.json:
//
//	{"version": 1, "arms": {"control": {"model": "model_a", "pct": 75},
//	                        "test":    {"model": "model_b", "pct": 25}}}
//
// The two percentages must sum to 100. The test percent is aligned to the nearest
// 1/16 by the router; control gets the remaining auctions. content.json is
// optional per model (a missing file means category-only signals); unknown keys
// are rejected so a typo cannot pass silently.
//
// Loading never fails the process. A missing ab-config.json means the built-in
// models at the baked percentage; any problem in the config or a model means the
// same fallback, with the first problem returned in Result.Err. The files are read
// once at startup.
package abmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/indexexchange/rtd-ab/internal/ab"
	"github.com/indexexchange/rtd-ab/internal/router"
)

// Sources of the loaded models, for rtd_ab_models_info.
const (
	SourceFile           = "file"
	SourceBuiltinAbsent  = "builtin_absent"
	SourceBuiltinInvalid = "builtin_invalid"
)

// BuiltinName is the model name reported when the built-in parameters are used.
const BuiltinName = "builtin"

const (
	dataDir      = "model_data"
	configFile   = "ab-config.json"
	maxFileBytes = 1 << 20
	maxEntries   = 5000
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Result is what Load found.
type Result struct {
	Source  string
	Control string // model directory name per arm, or BuiltinName
	Test    string
	// TestPct is the test arm's percent as written in the config, "" when the
	// built-in fallback is in use (the caller then uses the baked percentage).
	TestPct string
	Content [router.NumArms]ab.ContentModel
	Err     error // why the built-in fallback is in use, when Source is builtin_invalid
}

func builtin(source string, err error) Result {
	return Result{Source: source, Control: BuiltinName, Test: BuiltinName, Content: ab.DefaultContentModels(), Err: err}
}

type armSpec struct {
	Model string      `json:"model"`
	Pct   json.Number `json:"pct"`
}

type configDoc struct {
	Version int `json:"version"`
	Arms    struct {
		Control *armSpec `json:"control"`
		Test    *armSpec `json:"test"`
	} `json:"arms"`
}

type contentDoc struct {
	Sources  []string `json:"sources"`
	Genres   []string `json:"genres"`
	Keywords []string `json:"keywords"`
	Networks []string `json:"networks"`
}

// Load reads root/model_data/ab-config.json and the two models it names; root is
// the resolved model folder (the one holding manifest.json).
func Load(root string) Result {
	root = filepath.Join(root, dataDir)
	raw, err := readLimited(filepath.Join(root, configFile))
	if os.IsNotExist(err) {
		return builtin(SourceBuiltinAbsent, nil)
	}
	if err != nil {
		return builtin(SourceBuiltinInvalid, fmt.Errorf("%s: %w", configFile, err))
	}
	var cfg configDoc
	if err := decodeStrict(raw, &cfg); err != nil {
		return builtin(SourceBuiltinInvalid, fmt.Errorf("%s: %w", configFile, err))
	}
	if cfg.Version != 0 && cfg.Version != 1 {
		return builtin(SourceBuiltinInvalid, fmt.Errorf("%s: unsupported version %d (want 1)", configFile, cfg.Version))
	}
	if cfg.Arms.Control == nil || cfg.Arms.Test == nil {
		return builtin(SourceBuiltinInvalid, fmt.Errorf("%s: both arms.control and arms.test are required", configFile))
	}
	cBP, err := armPct("control", cfg.Arms.Control)
	if err != nil {
		return builtin(SourceBuiltinInvalid, err)
	}
	tBP, err := armPct("test", cfg.Arms.Test)
	if err != nil {
		return builtin(SourceBuiltinInvalid, err)
	}
	if cBP+tBP != router.MaxBP {
		return builtin(SourceBuiltinInvalid, fmt.Errorf("%s: pct values sum to %s, want 100", configFile, (cBP+tBP).Pct()))
	}

	res := Result{Source: SourceFile, TestPct: cfg.Arms.Test.Pct.String()}
	for _, a := range []struct {
		arm  router.Arm
		spec *armSpec
	}{{router.Control, cfg.Arms.Control}, {router.Test, cfg.Arms.Test}} {
		arm := a.arm
		cm, err := loadModel(root, a.spec.Model)
		if err != nil {
			return builtin(SourceBuiltinInvalid, fmt.Errorf("arm %s: %w", arm, err))
		}
		res.Content[arm] = cm
	}
	res.Control, res.Test = cfg.Arms.Control.Model, cfg.Arms.Test.Model
	return res
}

func armPct(arm string, a *armSpec) (router.BasisPoints, error) {
	if !nameRE.MatchString(a.Model) || a.Model == ".." {
		return 0, fmt.Errorf("%s: arms.%s.model %q is not a plain directory name", configFile, arm, a.Model)
	}
	bp, err := router.ParsePct(a.Pct.String())
	if err != nil {
		return 0, fmt.Errorf("%s: arms.%s.pct: %w", configFile, arm, err)
	}
	return bp, nil
}

func loadModel(root, name string) (ab.ContentModel, error) {
	dir := filepath.Join(root, name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ab.ContentModel{}, fmt.Errorf("model directory %q not found", name)
	}
	return loadContent(filepath.Join(dir, "content.json"), name)
}

// loadContent reads content.json. A missing file means category-only signals with
// the built-in allowlists.
func loadContent(path, model string) (ab.ContentModel, error) {
	def := ab.DefaultContentModels()[router.Control]
	raw, err := readLimited(path)
	if os.IsNotExist(err) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("%s/content.json: %w", model, err)
	}
	var d contentDoc
	if err := decodeStrict(raw, &d); err != nil {
		return def, fmt.Errorf("%s/content.json: %w", model, err)
	}
	if len(d.Sources) == 0 {
		return def, fmt.Errorf("%s/content.json: sources must list at least one of category, genre, keyword, network, eid", model)
	}
	m := ab.ContentModel{Genres: d.Genres, Keywords: d.Keywords, Networks: d.Networks}
	for _, n := range d.Sources {
		bit, ok := ab.SourceBit(n)
		if !ok {
			return def, fmt.Errorf("%s/content.json: unknown source %q", model, n)
		}
		m.Sources |= bit
	}
	for name, list := range map[string][]string{"genres": d.Genres, "keywords": d.Keywords, "networks": d.Networks} {
		if len(list) > maxEntries {
			return def, fmt.Errorf("%s/content.json: %s has %d entries, limit %d", model, name, len(list), maxEntries)
		}
	}
	return m, nil
}

func readLimited(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() || fi.Size() > maxFileBytes {
		return nil, fmt.Errorf("not a regular file of at most %d bytes", maxFileBytes)
	}
	return os.ReadFile(path)
}

// decodeStrict rejects unknown keys and trailing data, and keeps numbers exact.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if dec.More() {
		return fmt.Errorf("unexpected data after the JSON document")
	}
	return nil
}
