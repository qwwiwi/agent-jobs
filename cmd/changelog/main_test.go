package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oldChangelog = "# Changelog\n\n## [Unreleased]\n\n## [0.7.0] - 2026-11-07\n\n### Docs\n- Historical entry.\n"

func fixture(t *testing.T, fragments map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "changelog.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, root, "CHANGELOG.md", oldChangelog)
	for name, body := range fragments {
		put(t, root, "changelog.d/"+name, body)
	}
	return root
}

func put(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Requirements: R01 — sections and numeric issue order do not depend on directory order.
func TestAssembleGroupingOrdering(t *testing.T) {
	root := fixture(t, map[string]string{
		"100.added.md": "- Later (#100).\n",
		"9.added.md":   "- First (#9).\n- Second (#9).\n",
		"3.fixed.md":   "- Fixed (#3).\n",
		"2.docs.md":    "- Documented (#2).\n",
		"README.md":    "Not a fragment.\n",
	})
	var out bytes.Buffer
	if err := run([]string{"--root", root}, &out); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(oldChangelog, "## [Unreleased]\n\n", "## [Unreleased]\n\n### Added\n- First (#9).\n- Second (#9).\n- Later (#100).\n\n### Fixed\n- Fixed (#3).\n\n### Docs\n- Documented (#2).\n\n", 1)
	if got := read(t, root, "CHANGELOG.md"); got != want {
		t.Fatalf("assembled changelog:\n%s\nwant:\n%s", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(root, "changelog.d"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "README.md" {
		t.Fatalf("fragments not consumed or README removed: %v, %v", entries, err)
	}
	if err := run([]string{"--root", root}, &out); err != nil || read(t, root, "CHANGELOG.md") != want {
		t.Fatalf("second assembly changed the result: %v", err)
	}
}

func TestAssembleCheckAndValidateDoNotWrite(t *testing.T) {
	for _, flag := range []string{"--check", "--validate"} {
		t.Run(flag, func(t *testing.T) {
			root := fixture(t, map[string]string{"197.added.md": "- New (#197).\n"})
			var out bytes.Buffer
			if err := run([]string{"--root", root, flag}, &out); err != nil {
				t.Fatal(err)
			}
			if read(t, root, "CHANGELOG.md") != oldChangelog || read(t, root, "changelog.d/197.added.md") != "- New (#197).\n" {
				t.Fatal("read-only mode wrote files")
			}
			if flag == "--check" && !strings.Contains(out.String(), "### Added\n- New (#197).") {
				t.Fatalf("check did not print assembled result: %s", &out)
			}
		})
	}
}

func TestAssembleEmptyDir(t *testing.T) {
	root := fixture(t, nil)
	var out bytes.Buffer
	if err := run([]string{"--root", root, "--check"}, &out); err != nil || out.String() != oldChangelog {
		t.Fatalf("empty preview: %v, %q", err, out.String())
	}
	if err := run([]string{"--root", root}, &out); err != nil || read(t, root, "CHANGELOG.md") != oldChangelog {
		t.Fatalf("empty assembly changed changelog: %v", err)
	}
}

func TestAssembleBadFragment(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"197.unknown.md", "- Unknown (#197).\n"},
		{"issue.added.md", "- Bad name.\n"},
		{"0197.added.md", "- Ambiguous issue.\n"},
		{"197.added.txt", "- Wrong suffix.\n"},
		{"197.added.md", "### Added\n- Entry.\n"},
		{"197.fixed.md", "- Good.\nNot a bullet.\n"},
		{"197.removed.md", ""},
		{"197.security.md", "- \n"},
		{"197.changed.md", "- Good.\n\n- Blank line.\n"},
	} {
		t.Run(tc.name+tc.body, func(t *testing.T) {
			root := fixture(t, map[string]string{tc.name: tc.body, "1.added.md": "- Valid.\n"})
			for _, flags := range [][]string{{}, {"--check"}, {"--validate"}} {
				var out bytes.Buffer
				err := run(append([]string{"--root", root}, flags...), &out)
				if err == nil || !strings.Contains(err.Error(), tc.name) {
					t.Fatalf("bad fragment accepted or not named: %v", err)
				}
			}
			if read(t, root, "CHANGELOG.md") != oldChangelog || read(t, root, "changelog.d/1.added.md") != "- Valid.\n" {
				t.Fatal("invalid input caused partial assembly")
			}
		})
	}
}

func TestAssembleExistingUnreleased(t *testing.T) {
	root := fixture(t, map[string]string{"2.fixed.md": "- New fix.\n"})
	put(t, root, "CHANGELOG.md", strings.Replace(oldChangelog, "## [Unreleased]\n\n", "## [Unreleased]\n\n### Fixed\n- Existing fix.\n\n", 1))
	var out bytes.Buffer
	if err := run([]string{"--root", root, "--check"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "### Fixed\n- Existing fix.\n- New fix.\n") || strings.Count(out.String(), "### Fixed") != 1 {
		t.Fatalf("existing section was lost or duplicated:\n%s", &out)
	}
}

func TestAssembleMissingDir(t *testing.T) {
	root := t.TempDir()
	put(t, root, "CHANGELOG.md", oldChangelog)
	var out bytes.Buffer
	for _, flag := range []string{"--check", "--validate"} {
		if err := run([]string{"--root", root, flag}, &out); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != oldChangelog || read(t, root, "CHANGELOG.md") != oldChangelog {
		t.Fatal("absent fragments changed output")
	}
}

func TestAssembleMalformedChangelog(t *testing.T) {
	for _, body := range []string{
		"# Changelog\n",
		"## [Unreleased]\n\n## [Unreleased]\n",
		"## [Unreleased]\n\nRelease prose must not disappear.\n",
	} {
		root := fixture(t, map[string]string{"197.added.md": "- New.\n"})
		put(t, root, "CHANGELOG.md", body)
		var out bytes.Buffer
		if err := run([]string{"--root", root}, &out); err == nil {
			t.Fatalf("unsafe assembly accepted: %q", body)
		}
		if read(t, root, "CHANGELOG.md") != body || read(t, root, "changelog.d/197.added.md") != "- New.\n" {
			t.Fatal("failed assembly modified input")
		}
	}
}

func TestAssembleRejectsUnsafePaths(t *testing.T) {
	t.Run("fragment symlink", func(t *testing.T) {
		root := fixture(t, nil)
		put(t, root, "target.md", "- Outside fragment directory.\n")
		if err := os.Symlink("../target.md", filepath.Join(root, "changelog.d/197.added.md")); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := run([]string{"--root", root, "--validate"}, &out); err == nil {
			t.Fatal("symlink fragment accepted")
		}
	})
	t.Run("fragment directory entry", func(t *testing.T) {
		root := fixture(t, nil)
		if err := os.Mkdir(filepath.Join(root, "changelog.d/197.added.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := run([]string{"--root", root, "--validate"}, &out); err == nil {
			t.Fatal("directory fragment accepted")
		}
	})
}
