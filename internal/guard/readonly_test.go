package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readOnlySetup is a review job's layout: <jobdir>/work is the worktree, the call runs inside it.
func readOnlySetup(t *testing.T) (jobDir string, review, fix Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CDPATH", "")
	jobDir = filepath.Join(home, ".agent-jobs", "thrall", "jobs", "j")
	work := filepath.Join(jobDir, "work")
	if err := os.MkdirAll(filepath.Join(work, "internal"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"main.go", "a.go", "internal/x.go"} {
		if err := os.WriteFile(filepath.Join(work, f), []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fix = Config{Dir: work}
	review = fix
	review.ReadOnly = work
	return jobDir, review, fix
}

func isReadOnlyRefusal(r string) bool { return strings.Contains(r, "review job, read-only") }

// #39 made real: every write into the worktree that the deny rules' prefix patterns miss is refused
// by the guard in review mode, and none of them by this rule in fix mode.
func TestReadOnlyRefusesWorktreeWrites(t *testing.T) {
	jobDir, review, fix := readOnlySetup(t)
	work := review.ReadOnly
	for _, c := range []string{
		// redirections
		"echo x > out.txt",
		"echo x >> internal/x.go",
		"go test ./... &> log.txt",
		"printf 'x' > " + work + "/main.go",
		"cd internal && echo x > y.go",
		"echo x > \"$OUT\"",
		"exec 3> fd.txt",
		// tee, dd
		"cat a.go | tee b.go",
		"tee -a notes.md < a.go",
		"dd if=/dev/zero of=main.go bs=1 count=1",
		// in-place editors
		"sed -i 's/a/b/' main.go",
		"sed -i.bak -e 's/a/b/' main.go",
		"sed --in-place 's/a/b/' main.go",
		"sed -n 'w out.txt' main.go",
		"perl -i.bak -p fix.pl main.go",
		"ruby -i -p fix.rb main.go",
		"awk '{print > \"out.txt\"}' main.go",
		"awk '{system(\"rm main.go\")}' main.go",
		// tree writers
		"cp /tmp/x main.go",
		"cp /tmp/x .",
		"cp -t internal /tmp/x",
		"cp -r /tmp/x/work ..",
		"mv a.go b.go",
		"mv a.go /tmp/",
		"rm -rf internal",
		"rm -rf ..",
		"rm main.go",
		"install -m644 /tmp/x bin/x",
		"ln -s /etc/hosts hosts",
		"rsync -a /tmp/x/ ./",
		"tar -xf /tmp/a.tar",
		"tar xzf /tmp/a.tgz",
		"tar -czf out.tgz .",
		"unzip /tmp/a.zip",
		"patch -p1 < /tmp/x.diff",
		"touch new.go",
		"mkdir d",
		"chmod +x a.go",
		"truncate -s 0 a.go",
		"gzip a.go",
		"find . -name '*.orig' -delete",
		"find internal -exec rm {} +",
		"find internal -exec touch x.go ';'",
		"find . -fprint list.txt",
		"ls | xargs rm",
		"ls | xargs sed -i s/a/b/",
		// git writes in any spelling
		"git commit -m x",
		"git -C . commit -m x",
		"git -C " + work + " commit -qm x",
		"git --git-dir=.git --work-tree=. reset --hard",
		"git --no-pager merge main",
		"git rebase main",
		"git reset --hard",
		"git checkout -- a.go",
		"git checkout main",
		"git switch -c x",
		"git stash",
		"git apply /tmp/p.diff",
		"git am /tmp/p.mbox",
		"git cherry-pick abc",
		"git revert HEAD",
		"git restore a.go",
		"git clean -fd",
		"git add .",
		"git rm a.go",
		"git mv a.go b.go",
		"git update-ref refs/heads/x HEAD",
		"/usr/lib/git-core/git-commit -m x",
		"bash -c 'git commit -m x'",
		// interpreters with inline code, shells reading stdin
		"python3 -c 'open(\"x\",\"w\")'",
		"python3 -Ic pass",
		"python3 -cpass",
		"node -e 'require(\"fs\")'",
		"node --eval 1",
		"node --input-type=module -e 1",
		"perl -e 1",
		"perl -pi -e 's/a/b/' main.go",
		"ruby -e 1",
		"php -r 1;",
		"python3 <<'EOF'\nopen('x','w')\nEOF",
		"python3 - <<< 'x'",
		"echo 'open(1)' | python3",
		"bash <<'EOF'\necho x > y\nEOF",
		"cat s.sh | sh",
		"bash -c 'echo x > y'",
		"sh -s < s.sh",
	} {
		// A form an earlier rule refuses in every kind (a glob in a writer's argument, a shell
		// reading stdin, git inside sh -c) stays refused for that reason.
		r, rf := Check(c, review), Check(c, fix)
		if !isReadOnlyRefusal(r) && (r == "" || rf == "") {
			t.Errorf("review: %q not refused as a worktree write (reason %q)", c, r)
		}
		if isReadOnlyRefusal(rf) {
			t.Errorf("fix: %q refused by the read-only rule: %s", c, rf)
		}
	}
	// File tools by path, through a symlink from the job dir too.
	if err := os.Symlink(work, filepath.Join(jobDir, "w")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"main.go", "new/file.go", work + "/internal/x.go", filepath.Join(jobDir, "w", "a.go")} {
		for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
			if r := CheckFileWrite(tool, p, review); !isReadOnlyRefusal(r) {
				t.Errorf("review: %s %s not refused (reason %q)", tool, p, r)
			}
			if r := CheckFileWrite(tool, p, fix); r != "" {
				t.Errorf("fix: %s %s refused: %s", tool, p, r)
			}
		}
	}
}

// Reads, tests and writes outside the worktree (the job directory, /tmp) stay allowed in review mode.
func TestReadOnlyAllowsReadsTestsAndJobDirWrites(t *testing.T) {
	jobDir, review, _ := readOnlySetup(t)
	for _, c := range []string{
		"go test ./...",
		"go test -count=1 ./... 2>&1 | tail -20",
		"go vet ./...",
		"npm test",
		"pytest -q",
		"python3 -m pytest -q",
		"python3 -W ignore -m pytest",
		"python3 ../check.py main.go",
		"cat main.go | python3 ../count.py",
		"node --test",
		"git diff",
		"git diff main...HEAD -- a.go",
		"git -C . log --oneline -5",
		"git status --porcelain",
		"git show HEAD:main.go",
		"git branch --show-current",
		"cat main.go",
		"grep -rn x .",
		"sed -n 1,10p main.go",
		"sed 's/a/b/' main.go > ../out.go",
		"sed -i 's/a/b/' ../notes.md",
		"awk 'NR==1' main.go",
		"awk '$1 > 5' main.go",
		"echo x > /dev/null",
		"echo notes > ../notes.md",
		"echo x >> " + jobDir + "/log.txt",
		"go test ./... > /tmp/test.log 2>&1",
		"cp main.go ../main.go.orig",
		"cp -r internal /tmp/copy",
		"mkdir -p ../tmp",
		"go test ./... | tee ../test.log",
		"tar -tf ../a.tar",
		"tar -czf ../src.tgz .",
		"tar -xf ../a.tar -C ../unpacked",
		"find . -name main.go",
		"find internal -exec grep -l x ';'",
		"find . -name '*.go' | xargs grep -n x",
		"ls -la",
		"rm ../tmp/x",
		"bash ../run.sh",
		"bash -c 'go test ./...'",
	} {
		if r := Check(c, review); r != "" {
			t.Errorf("review: %q refused: %s", c, r)
		}
	}
	for _, p := range []string{filepath.Join(jobDir, "notes.md"), "../notes.md.tmp", "/tmp/x"} {
		if strings.Contains(p, "..") {
			continue // `..` paths are refused by CheckFileWrite on their own
		}
		if r := CheckFileWrite("Write", p, review); r != "" {
			t.Errorf("review: Write %s refused: %s", p, r)
		}
	}
}

// Decide passes the read-only worktree to the file tools as well as to Bash.
func TestReadOnlyDecide(t *testing.T) {
	_, review, _ := readOnlySetup(t)
	for _, in := range []string{
		`{"tool_name":"Write","tool_input":{"file_path":"main.go","content":"x"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"sed -i s/a/b/ main.go"}}`,
	} {
		r, err := Decide(strings.NewReader(in), review)
		if err != nil || !isReadOnlyRefusal(r) {
			t.Errorf("Decide(%s) = %q, %v", in, r, err)
		}
	}
}
