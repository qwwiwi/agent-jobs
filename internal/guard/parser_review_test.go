package guard

import (
	"archive/tar"
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review-only regression specifications. Commands are passed to Check, never executed.
func reviewCases(t *testing.T, cfg Config, cases []struct {
	name, cmd string
	block     bool
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := Check(tc.cmd, cfg)
			if (reason != "") != tc.block {
				t.Errorf("Check(%q) = %q; want block=%v", tc.cmd, reason, tc.block)
			}
		})
	}
}

func TestReviewHard2Runners(t *testing.T) {
	reviewCases(t, Config{}, []struct {
		name, cmd string
		block     bool
	}{
		{"vim_cluster", `vim -esc '!screen'`, true},
		{"vim_execute", `vim -c 'execute "!screen"'`, true},
		{"vim_absolute", `/usr/bin/vim -c '!screen'`, true},
		{"script_attached", `script -qc'screen -ls' /dev/null`, true},
		{"script_long", `script --command='screen -ls' /dev/null`, true},
		{"watch_cluster", `watch -tn 2 'screen -ls'`, true},
		{"watch_absolute", `/usr/bin/watch -n 2 'screen -ls'`, true},
		{"xargs_long_value", `xargs --max-args 1 screen`, true},
		{"xargs_attached_value", `xargs -In screen`, true},
		{"nice_long_value", `nice --adjustment 5 screen`, true},
		{"timeout_long_value", `timeout --signal TERM 2 screen`, true},
		{"env_attached_value", `env -CsrcS screen`, true},
		{"stdbuf_long_value", `stdbuf --output L screen`, true},
		{"nested_find", `find . -exec /usr/bin/env /usr/bin/nice --adjustment 5 screen \;`, true},
		{"nested_busybox", `busybox xargs -In screen`, true},
		{"nohup_absolute", `/usr/bin/nohup /usr/bin/screen -ls`, true},
		{"setsid_absolute", `/usr/bin/setsid /usr/bin/screen -ls`, true},
		{"perl_q", `perl -e 'system q(screen)'`, true},
		{"perl_block", `perl -e 'system {"/usr/bin/screen"} "screen", "-ls"'`, true},
		{"awk_e", `gawk -e 'BEGIN {system("screen")}'`, true},
		{"awk_long", `gawk --source 'BEGIN {system("screen")}'`, true},
	})
}

func TestReviewHard2QuotedPaths(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "a b,c")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: root, Protected: []string{filepath.Join(state, "jobs.db")}}
	reviewCases(t, cfg, []struct {
		name, cmd string
		block     bool
	}{
		{"python_literal", `python3 -c "open('./a b,c/./jobs.db','w')"`, true},
		{"python_nested_command", `python3 -c 'import os; os.system("printf x > \"./a b,c/./jobs.db\"")'`, true},
		{"python_triple_quote", `python3 -c 'open("""./a b,c/./jobs.db""", "w")'`, true},
		{"sqlite_double_quote", `sqlite3 :memory: 'ATTACH DATABASE "file:./a b,c/./jobs.db?mode=rw" AS x'`, true},
		{"sqlite_percent_uri", `sqlite3 :memory: "ATTACH DATABASE 'file:./a%20b%2Cc/./jobs.db?mode=rw' AS x"`, true},
	})
}

func TestReviewHard2Links(t *testing.T) {
	state, _, cfg := stateReadSetup(t)
	src := t.TempDir()
	if err := os.Symlink("../../../../thrall", filepath.Join(src, "L")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("safe-target", filepath.Join(src, "safe")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "build"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "L"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	// The archive can replace an existing regular file, introduce a direct file
	// symlink, or introduce a directory link. No extraction is executed here.
	f, err := os.Create(filepath.Join(cfg.Dir, "a.tar"))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, item := range []struct{ name, target string }{{"L", state}, {"copy", filepath.Join(state, "jobs.db")}, {"X", state}} {
		if err := tw.WriteHeader(&tar.Header{Name: item.name, Linkname: item.target, Typeflag: tar.TypeSymlink, Mode: 0777}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	zf, err := os.Create(filepath.Join(cfg.Dir, "a.zip"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	zh := &zip.FileHeader{Name: "copy"}
	zh.SetMode(os.ModeSymlink | 0777)
	zentry, err := zw.CreateHeader(zh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zentry.Write([]byte(state)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, producer, target string }{
		{"cp_long_recursive", "cp --recursive %s/L copy", "copy/jobs.db"},
		{"cp_abbrev_archive", "cp --arch %s/L copy", "copy/jobs.db"},
		{"rsync_t_cluster", "rsync -alt %s/L copy", "copy/jobs.db"},
		{"rsync_t_separate", "rsync -a -t %s/L copy", "copy/jobs.db"},
		{"cp_chain", "cp -P %s/L build/L; cp -P build/L copy", "copy/jobs.db"},
		{"mv_chain", "cp -P %s/L build/L; mv build/L copy", "copy/jobs.db"},
		{"cp_overwritten_history", "cp -P %s/L copy; printf x > copy/jobs.db; cp -Pf %s/safe copy", "copy/jobs.db"},
		{"tar_existing_component", "tar xf a.tar", "L/jobs.db"},
		{"tar_direct_file_link", "tar xf a.tar", "copy"},
		{"tar_oldstyle_C", "tar xCf build a.tar", "build/X/jobs.db"},
		{"tar_nested_C", "mkdir -p build/sub; tar -xf a.tar -C build -C sub", "build/sub/X/jobs.db"},
		{"unzip_password", "unzip -Psecret a.zip", "copy/jobs.db"},
		{"cp_known", "cp -P %s/L copy", "copy/jobs.db"},
		{"install_known", "cp -P %s/L copy; install /dev/null copy/jobs.db", "copy/jobs.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer := tc.producer
			if strings.Contains(producer, "%s") {
				producer = strings.ReplaceAll(producer, "%s", src)
			}
			cmd := producer + "; printf x > " + tc.target
			if reason := Check(cmd, cfg); reason == "" {
				t.Errorf("Check(%q) allowed", cmd)
			}
		})
	}
	reviewCases(t, cfg, []struct {
		name, cmd string
		block     bool
	}{
		{"tar_read_state", `tar xf a.tar; cat X/jobs.db`, true},
		{"tar_write_config", `tar xf a.tar; printf x > X/config.json`, true},
		{"python_symlink_literal", fmt.Sprintf(`python3 -c "import os; os.symlink('%s','copy')"`, state), true},
		{"install_literal", fmt.Sprintf(`install /dev/null %s/jobs.db`, state), true},
	})
}

func TestReviewHard2Feeders(t *testing.T) {
	_, _, _, cfg := protectedSetup(t)
	reviewCases(t, cfg, []struct {
		name, cmd string
		block     bool
	}{
		{"group_receiver", `printf '%s' "$HOME" | { python3; }`, true},
		{"group_program", `printf 'open("%s/.agent-jobs/thrall/jobs.db","w").write("x")' "$HOME" | { python3; }`, true},
		{"subshell_receiver", `printf '%s' "$HOME" | (python3)`, true},
		{"nested_receiver", `printf '%s' "$HOME" | { { python3; }; }`, true},
		{"redirect_group", `{ python3; } < <(printf '%s' "$HOME")`, true},
		{"nodejs", `printf '%s' "$HOME" | /usr/bin/nodejs`, true},
		{"lua_version", `printf '%s' "$HOME" | /usr/bin/lua5.4`, true},
		{"perl_version", `printf '%s' "$HOME" | /usr/bin/perl5.38.2`, true},
		{"ruby_version", `printf '%s' "$HOME" | /usr/bin/ruby3.2`, true},
		{"php_version", `printf '%s' "$HOME" | /usr/bin/php8.3`, true},
	})
}

func TestReviewHard2DeveloperCommands(t *testing.T) {
	_, _, cfg := stateReadSetup(t)
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "src", "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewCases(t, cfg, []struct {
		name, cmd string
		block     bool
	}{
		{"go_test", `go test ./...`, false},
		{"go_test_named", `go test -run Screen ./...`, false},
		{"git_status", `git status --short`, false},
		{"git_diff", `git diff -- src/main.go`, false},
		{"find_name", `find src -name '*.go'`, false},
		{"xargs_grep", `find src -name '*.go' -print0 | xargs -0 grep -n package`, false},
		{"xargs_group", `{ find src -name '*.go' -print0; } | xargs -0 grep -n package`, false},
		{"tar_create", `tar -cf build.tar src`, false},
		{"tar_create_x_filename", `tar -cfexample.tar src; printf x > new/jobs.db`, false},
		{"cp_recursive", `cp -r src copy`, false},
		{"python_repo", `python3 -c "print(open('src/main.go').read())"`, false},
		{"python_repo_write", `python3 -c "open('src/output.txt','w').write('ok')"`, false},
		{"python_heredoc", "python3 <<'PY'\nprint(open('src/main.go').read())\nPY", false},
		{"python_heredoc_divide", "python3 <<'PY'\nprint(1 / 2)\nPY", false},
		{"python_heredoc_dot", "python3 <<'PY'\nprint('.')\nPY", false},
		{"perl_quoted_data", `perl -e 'print q(system("screen"))'`, false},
		{"awk_regex_data", `awk '/system("screen")/' src/main.go`, false},
		{"vim_quoted_data", `vim -c 'let msg = ":!screen"'`, false},
	})
}
