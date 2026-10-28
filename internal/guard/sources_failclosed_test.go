package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fix loop 2 of G02: in a call that touches the sources or the sandbox the guard is fail-closed, so a new
// spelling of a refused command is refused too: options outside a known-safe set, expansions it cannot
// resolve in a call that writes outside the sandbox, dynamic or linked git dirs.
func TestSourcesFailClosed(t *testing.T) {
	jobDir, cfg := sourcesSetup(t)
	refused := func(t *testing.T, cases []string) {
		t.Helper()
		for _, c := range cases {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
	}
	allowed := func(t *testing.T, cases []string) {
		t.Helper()
		for _, c := range cases {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	}
	t.Run("[G02] a copier given a source path takes only known-safe options; ln of a source is refused", func(t *testing.T) {
		refused(t, []string{
			"cp -S -- -l ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp -S x -l ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp --suffix=-- -l ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp -b ../sources/lib/install.sh ../sandbox/b.sh",
			"cp --backup=numbered ../sources/lib/install.sh ../sandbox/b.sh",
			"cp -u ../sources/lib/install.sh ../sandbox/u.sh",
			"cp -Rx ../sources/lib ../sandbox/lx",
			"cp -t ../sandbox ../sources/lib/install.sh",
			"cp --li ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp --rec ../sources/lib ../sandbox/lr",
			"install ../sources/lib/install.sh ../sandbox/i.sh",
			"install -m 755 ../sources/lib/install.sh ../sandbox/i.sh",
			"rsync -aH ../sources/lib/ ../sandbox/l3/",
			"rsync -a --copy-dest=../sources/lib ../sources/lib/ ../sandbox/l3/",
			"rsync -a --exclude x ../sources/lib/ ../sandbox/l3/",
			"ln -s ../../sources/lib/bin ../tmp/c",
			"ln -t ../sandbox ../sources/lib/install.sh",
			"ln -sf -- ../sources/lib/install.sh ../sandbox/l.sh",
		})
		allowed(t, []string{
			"cp -R ../sources/lib ../sandbox/lib",
			"cp -r ../sources/lib ../sandbox/lib2",
			"cp -a ../sources/lib ../sandbox/lib3",
			"cp -Rpv ../sources/lib ../sandbox/lib4",
			"cp -RL ../sources/lib ../sandbox/lib5",
			"cp -R -- ../sources/lib ../sandbox/lib6",
			"cp --recursive --preserve=mode,timestamps ../sources/lib ../sandbox/lib7",
			"cp -R --no-preserve=ownership ../sources/lib ../sandbox/lib8",
			"cp -T -R ../sources/lib ../sandbox/lib9",
			"cp ../sources/lib/install.sh ../sandbox/lib/i.sh",
			"rsync -a ../sources/lib/ ../sandbox/r1/",
			"rsync -av --exclude=node_modules ../sources/lib/ ../sandbox/r2/",
			"cp -l work-a work-b",
			"ln -s main.go m.go",
		})
	})
	t.Run("[G02] a call writing outside the sandbox resolves every word: no glob, brace, variable or ~user", func(t *testing.T) {
		refused(t, []string{
			"cat < ../source[s]/lib/install.sh > ../tmp/x.sh",
			"cat < ../sources/li?/install.sh > ../tmp/x.sh",
			"cat ../sour{c,c}es/lib/install.sh > ../tmp/x.sh",
			"cat ../sou{r..r}ces/lib/install.sh > ../tmp/x.sh",
			"head -c 9999 ../[s]ources/lib/install.sh >> ../tmp/x.sh",
			"cat ../[!x]ources/lib/install.sh > ../tmp/x.sh",
			"cat ../*/lib/install.sh > ../tmp/x.sh",
			"cat \"$F\" > ../tmp/x.sh",
			"F=../sources/lib/install.sh; cat $F > ../tmp/x.sh",
			"cat $(echo ../sourc)es/lib/install.sh > ../tmp/x.sh",
			"cat `echo ../sourc`es/lib/install.sh > ../tmp/x.sh",
			"cat ~nobody/x > ../tmp/x.sh",
			"cat < \"$F\" > ../tmp/x.sh",
			"cat ../sour{c,c}es/lib/install.sh | tee ../tmp/x.sh",
			"iconv -f utf-8 -t utf-8 -o ../tmp/x.sh ../sour{c,c}es/lib/install.sh",
			"cp /dev/stdin ../tmp/x.sh < ../source[s]/lib/install.sh",
			"echo \"$(cat ../sour{c,c}es/lib/install.sh)\" > ../tmp/x.sh",
			"cat ../sour{c,c}es/lib/install.sh > \"$OUT\"",
		})
		allowed(t, []string{
			"cat ../sour{c,c}es/lib/install.sh",
			"cat ../sources/lib/*.sh",
			"cat ../sources/lib/*.sh > ../sandbox/lib/all.sh",
			"cat < ../sources/lib/install.sh",
			"cat ../sources/lib/install.sh > /dev/null",
			"echo \"$HOME\" > ../tmp/log",
			"echo \"$X\" > ../tmp/log",
			"printf '%s\\n' \"$X\" >> ../tmp/log",
			"go test ./... > ../tmp/test.log 2>&1",
			"git diff > ../tmp/d.patch",
		})
	})
	t.Run("[G02] git with a dynamic or linked git dir, work tree or -C is refused", func(t *testing.T) {
		box := filepath.Join(jobDir, "tmp", "y")
		if err := os.MkdirAll(box, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../sources/lib/bin", filepath.Join(box, "c")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(jobDir, "sources", "lib"), filepath.Join(box, "w")); err != nil {
			t.Fatal(err)
		}
		refused(t, []string{
			"D=$(printf ../sourc%s/lib/bin es); git --git-dir=\"$D\" log -p",
			"git --git-dir=\"$(printf ../sourc%s/lib/bin es)\" log -p",
			"git --git-dir \"$D\" log -p",
			"git --work-tree=\"$D\" status",
			"git -C \"$D\" log -p",
			"git -C $(printf ../sourc%s/lib es) log -p",
			"ln -s ../../sources/lib/bin ../tmp/y/d; git --git-dir=../tmp/y/d log -p",
			"ln -s x ../tmp/y/e && git --git-dir=../tmp/y/e log -p",
			"ln -s x ../tmp/y/f; git log -p",
			"git --git-dir=../tmp/y/c log -p",
			"git --git-dir ../tmp/y/c log -p",
			"git --work-tree=../tmp/y/w status",
			"git --git-dir=../tmp/y/nothere log -p",
			"git --git-dir=.git log; cat ../sources/lib/install.sh",
			"GIT_DIR=\"$D\" git log -p",
			"GIT_DIR=../sources/lib/bin git log -p",
			"env GIT_DIR=../sources/lib/bin git log -p",
			"env GIT_WORK_TREE=../sources/lib git status",
			"export GIT_DIR=../tmp/y/c; git log -p",
		})
		allowed(t, []string{
			"git -C ../sources/lib log -p",
			"cd ../sources/lib && git log -p",
			"git log -p",
			"git -C . status",
		})
	})
	t.Run("[G02] here-docs and echo/printf text that mention the sources are not reads of them", func(t *testing.T) {
		allowed(t, []string{
			"cat > ../PROGRESS.md <<'X'\n- grep in sources/lib done\nX",
			"cat > ../PROGRESS.md <<'X'\n- read ../sources/lib/install.sh, copied to ../sandbox/lib\nX",
			"cat >> ../PROGRESS.md <<X\n- read ../sources/lib/install.sh\nX",
			"echo '- read ../sources/lib/install.sh' >> ../PROGRESS.md",
			"echo \"- read ../sources/lib/install.sh\" >> ../PROGRESS.md",
			"printf '%s\\n' '- read ../sources/lib/install.sh' >> ../PROGRESS.md",
			"tee ../PROGRESS.md <<'X'\n- grep in ../sources/lib done\nX",
			"cat <<< '../sources/lib/install.sh' > ../tmp/note",
		})
		refused(t, []string{
			"cat > ../tmp/x.sh <<X\n$(cat ../sources/lib/install.sh)\nX",
			"printf '%s' \"$(cat ../sources/lib/install.sh)\" > ../tmp/x.sh",
			"echo ../sources/lib/install.sh | xargs cat > ../tmp/x.sh",
			"bash <<'X'\n../sources/lib/install.sh\nX",
		})
	})
	t.Run("[G02] refusals of unresolved forms point to the allowed recipe", func(t *testing.T) {
		r := Check("cat ../sour{c,c}es/lib/install.sh > ../tmp/x.sh", cfg)
		if !strings.Contains(r, "cannot resolve") || !strings.Contains(r, "docker run --rm") {
			t.Errorf("want a fail-closed refusal with the recipe, got %q", r)
		}
	})
}
