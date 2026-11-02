// SPDX-License-Identifier: Apache-2.0

package abmodel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/indexexchange/rtd-ab/internal/ab"
	"github.com/indexexchange/rtd-ab/internal/router"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, "model_data", rel) // all content lives under model_data/
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goodConfig = `{"version":1,"arms":{"control":{"model":"model_a","pct":75},"test":{"model":"model_b","pct":25}}}`

func goodRoot(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "ab-config.json", goodConfig)
	write(t, root, "model_a/content.json", `{"sources":["category","genre"],"genres":["Polka"]}`)
	write(t, root, "model_b/content.json", `{"sources":["keyword","eid"],"keywords":["zzz"]}`)
	return root
}

func TestLoadsModelsAndMix(t *testing.T) {
	r := Load(goodRoot(t))
	if r.Err != nil || r.Source != SourceFile || r.Control != "model_a" || r.Test != "model_b" || r.TestPct != "25" {
		t.Fatalf("%+v", r)
	}
	if r.Content[router.Control].Sources.Names() != "category+genre" || r.Content[router.Test].Sources.Names() != "keyword+eid" ||
		r.Content[router.Control].Genres[0] != "Polka" || r.Content[router.Test].Keywords[0] != "zzz" {
		t.Fatalf("content: %+v", r.Content)
	}
}

// A config at the top of the folder, outside model_data/, is not read.
func TestConfigOutsideModelDataIsIgnored(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ab-config.json"), []byte(goodConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := Load(root); r.Source != SourceBuiltinAbsent {
		t.Fatalf("%+v", r)
	}
}

func TestMissingConfigFallsBackToBuiltin(t *testing.T) {
	r := Load(t.TempDir())
	if r.Source != SourceBuiltinAbsent || r.Err != nil || r.Control != BuiltinName || r.TestPct != "" {
		t.Fatalf("%+v", r)
	}
	if r.Content[router.Test].Sources.Names() != "category+genre+keyword+network+eid" {
		t.Fatal("built-in models not applied")
	}
	if r := Load(filepath.Join(t.TempDir(), "does-not-exist")); r.Source != SourceBuiltinAbsent {
		t.Fatalf("missing dir: %+v", r)
	}
}

func TestMissingModelFilesMeanBuiltinParameters(t *testing.T) {
	root := t.TempDir()
	write(t, root, "ab-config.json", goodConfig)
	write(t, root, "model_a/.keep", "")
	write(t, root, "model_b/.keep", "")
	r := Load(root)
	if r.Err != nil || r.Source != SourceFile {
		t.Fatalf("%+v", r)
	}
	if r.Content[router.Test].Sources.Names() != "category" {
		t.Fatalf("a model without files runs category-only signals: %+v", r.Content[router.Test])
	}
}

// Every problem falls back to the built-in models with one error, never a crash.
func TestInvalidConfigsFallBack(t *testing.T) {
	cases := map[string]func(root string){
		"broken json": func(r string) { write(t, r, "ab-config.json", `{`) },
		"unknown key": func(r string) {
			write(t, r, "ab-config.json", strings.Replace(goodConfig, `"pct":25`, `"pct":25,"pcts":1`, 1))
		},
		"trailing data": func(r string) { write(t, r, "ab-config.json", goodConfig+`{}`) },
		"bad version": func(r string) {
			write(t, r, "ab-config.json", strings.Replace(goodConfig, `"version":1`, `"version":2`, 1))
		},
		"missing arm": func(r string) { write(t, r, "ab-config.json", `{"arms":{"control":{"model":"model_a","pct":100}}}`) },
		"pct sum":     func(r string) { write(t, r, "ab-config.json", strings.Replace(goodConfig, `"pct":25`, `"pct":30`, 1)) },
		"pct decimals": func(r string) {
			write(t, r, "ab-config.json", strings.Replace(goodConfig, `"pct":25`, `"pct":25.123`, 1))
		},
		"pct exponent": func(r string) {
			write(t, r, "ab-config.json", strings.Replace(goodConfig, `"pct":25`, `"pct":2.5e1`, 1))
		},
		"pct missing":    func(r string) { write(t, r, "ab-config.json", strings.Replace(goodConfig, `,"pct":25`, ``, 1)) },
		"path traversal": func(r string) { write(t, r, "ab-config.json", strings.Replace(goodConfig, `model_b`, `../x`, 1)) },
		"slash in name":  func(r string) { write(t, r, "ab-config.json", strings.Replace(goodConfig, `model_b`, `a/b`, 1)) },
		"missing model":  func(r string) { os.RemoveAll(filepath.Join(r, "model_data", "model_b")) },
		"no sources":     func(r string) { write(t, r, "model_b/content.json", `{"sources":[]}`) },
		"unknown source": func(r string) { write(t, r, "model_b/content.json", `{"sources":["cookies"]}`) },
		"huge file": func(r string) {
			write(t, r, "model_b/content.json", `{"sources":["genre"],"genres":["`+strings.Repeat("a", 1<<21)+`"]}`)
		},
		"config is a dir": func(r string) {
			os.Remove(filepath.Join(r, "model_data", "ab-config.json"))
			os.Mkdir(filepath.Join(r, "model_data", "ab-config.json"), 0o755)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := goodRoot(t)
			mutate(root)
			r := Load(root)
			if r.Source != SourceBuiltinInvalid || r.Err == nil || r.Control != BuiltinName || r.Test != BuiltinName || r.TestPct != "" {
				t.Fatalf("%+v", r)
			}
			if d := ab.DefaultContentModels(); r.Content[router.Control].Sources != d[router.Control].Sources || r.Content[router.Test].Sources != d[router.Test].Sources {
				t.Fatal("fallback must use the built-in models")
			}
			t.Log(r.Err)
		})
	}
}

func TestTooManyAllowlistEntries(t *testing.T) {
	root := goodRoot(t)
	list := `"` + strings.Repeat(`a","`, maxEntries) + `a"`
	write(t, root, "model_b/content.json", `{"sources":["genre"],"genres":[`+list+`]}`)
	if r := Load(root); r.Source != SourceBuiltinInvalid || !strings.Contains(r.Err.Error(), "limit") {
		t.Fatalf("%+v", r)
	}
}
