package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Fix loop 1 of the review of 0ac658d: git on a bare repository inside a clone, hard links into the
// sandbox, copies out through redirections and git, a container that outlives the job.
func TestSourcesReviewBlockers(t *testing.T) {
	jobDir, cfg := sourcesSetup(t)
	box := filepath.Join(jobDir, "sandbox")
	refused := func(t *testing.T, cases []string) {
		t.Helper()
		for _, c := range cases {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
	}
	t.Run("[G02] git works on a clone only at its root, never on a git dir or work tree under sources", func(t *testing.T) {
		refused(t, []string{
			"cd ../sources/lib/bin && git log -p",
			"cd ../sources/lib/bin; git show",
			"pushd ../sources/lib/bin && git log -p",
			"git -C ../sources/lib/bin log -p",
			"git -C " + jobDir + "/sources/lib/bin log -p",
			"git -C ../sources/lib -C bin log -p",
			"git -C ../sources log",
			"cd ../sources && git log",
			"git --git-dir=../sources/lib/bin log -p",
			"git --git-dir ../sources/lib/bin log -p",
			"git --git-dir=../sources/lib/.git log",
			"git --work-tree=../sources/lib status",
			"git -C ../sources/lib --work-tree=../../tmp checkout HEAD -- .",
			"git -C ../sources/lib --git-dir=.git log",
		})
	})
	t.Run("[G02] links of source files are refused: a copy into the sandbox is a real copy", func(t *testing.T) {
		refused(t, []string{
			"cp -l ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp --link ../sources/lib/install.sh ../sandbox/linked.sh",
			"cp -al ../sources/lib ../sandbox/l2",
			"cp -Rl ../sources/lib ../sandbox/l2",
			"cp -s ../sources/lib/install.sh ../sandbox/s.sh",
			"cp --symbolic-link ../sources/lib/install.sh ../sandbox/s.sh",
			"ln ../sources/lib/install.sh ../sandbox/linked.sh",
			"ln -f ../sources/lib/install.sh ../sandbox/linked.sh",
			"ln -s ../sources/lib/install.sh ../sandbox/s.sh",
			"rsync -a --link-dest=../sources/lib ../sources/lib/ ../sandbox/l3/",
			"rsync -a --link-dest ../sources/lib ../sources/lib/ ../sandbox/l3/",
		})
	})
	t.Run("[G02] copies out through redirections, output options and git are refused", func(t *testing.T) {
		refused(t, []string{
			"cat ../sources/lib/install.sh > ../tmp/x.sh",
			"cat ../sources/lib/install.sh >> ../tmp/x.sh",
			"cat ../sources/lib/install.sh 1> ../tmp/x.sh",
			"cat ../sources/lib/install.sh &> ../tmp/x.sh",
			"cat ../sources/lib/install.sh >| ../tmp/x.sh",
			"head -100 ../sources/lib/install.sh > ../tmp/x.sh",
			"tail -n +1 ../sources/lib/install.sh > ../tmp/x.sh",
			"base64 ../sources/lib/install.sh > ../tmp/x.b64",
			"cat < ../sources/lib/install.sh > ../tmp/x.sh",
			"cat ../sandbox/lib/run.sh > ../work/x",
			"cat ../sources/lib/install.sh | cat > ../tmp/x.sh",
			"cat ../sources/lib/install.sh | tee ../tmp/x.sh",
			"{ cat ../sources/lib/install.sh; } > ../tmp/x.sh",
			"( cat ../sources/lib/install.sh ) > ../tmp/x.sh",
			"for f in ../sources/lib/install.sh; do cat $f; done > ../tmp/x.sh",
			"exec > ../tmp/x.sh; cat ../sources/lib/install.sh",
			"printf '%s' \"$(cat ../sources/lib/install.sh)\" > ../tmp/x.sh",
			"cd ../sources/lib && cat install.sh > ../../tmp/x.sh",
			"iconv -f utf-8 -t utf-8 ../sources/lib/install.sh -o ../tmp/x.sh",
			"iconv -f utf-8 -t utf-8 --output=../tmp/x.sh ../sources/lib/install.sh",
			"uniq ../sources/lib/install.sh ../tmp/x.sh",
			"git -C ../sources/lib archive -o ../../tmp/a.tar HEAD",
			"git -C ../sources/lib archive HEAD > ../tmp/a.tar",
			"git -C ../sources/lib show HEAD:install.sh > ../tmp/x.sh",
			"git -C ../sources/lib log -p --output=../../tmp/x",
			"git -C ../sources/lib checkout-index -a --prefix=../../tmp/x/",
			"git -C ../sources/lib bundle create ../../tmp/b.bundle --all",
			"git -C ../sources/lib worktree add ../../tmp/w",
			"git clone ../sources/lib ../tmp/c",
			"git clone " + jobDir + "/sources/lib ../tmp/c",
			"git fetch ../sources/lib",
			"git archive --remote=../sources/lib HEAD",
		})
	})
	t.Run("[G02] cross-review: --bare, hidden directories, ancestors, rsync operands, stdin copies, globs", func(t *testing.T) {
		refused(t, []string{
			"git -C ../sources/lib --bare log -p",
			"cd ../sources/lib && git --bare diff HEAD~ HEAD",
			"cd \"$X\" && git --git-dir=. log -p",
			"git -C \"$X\" --bare log -p",
			"cp -R .. ../tmp/jd",
			"tar -cf ../tmp/x.tar ..",
			"rsync -a ../ ../tmp/jd",
			"rsync -a ../sources/lib/ ../tmp/x --exclude ../sandbox/z",
			"rsync -a --backup-dir=../tmp/b ../sources/lib/ ../sandbox/lib/",
			"cp /dev/stdin ../tmp/x.sh < ../sources/lib/install.sh",
			"cat ../sources/lib/install.sh | cp /dev/stdin ../tmp/x.sh",
			"cat ../source[s]/lib/install.sh > ../tmp/x.sh",
			"cat ../sources/li?/install.sh > ../tmp/x.sh",
		})
	})
	t.Run("[G02] reading the clones and the sandbox route stay allowed", func(t *testing.T) {
		user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		for _, c := range []string{
			"cat ../sources/lib/install.sh",
			"grep -rn main ../sources/lib",
			"grep -o main ../sources/lib/setup.py",
			"rg -n main ../sources/lib",
			"ls -la ../sources/lib",
			"head -5 ../sources/lib/setup.py",
			"find ../sources/lib -name '*.sh'",
			"cat ../sources/lib/install.sh 2>/dev/null",
			"grep -rn main ../sources/lib 2>&1 | head",
			"cat ../sources/lib/install.sh > /dev/null",
			"cat ../sources/lib/install.sh > ../sandbox/lib/x.sh",
			"cd ../sources/lib && git log",
			"cd ../sources/lib && git log -p",
			"git -C ../sources/lib log -p",
			"git -C " + jobDir + "/sources/lib show HEAD --stat",
			"git -C ../sources/lib diff HEAD --stat",
			"git -C ../sources/lib grep main",
			"git -C ../sources/lib ls-files",
			"git -C ../sources/lib blame install.sh",
			"echo done > ../tmp/log",
			"git status",
			"cp -R ../sources/lib ../sandbox/lib",
			"cp -a ../sources/lib ../sandbox/lib4",
			"chmod -R u+w ../sandbox/lib",
			"docker run --rm --name aj-j-test --memory 2g --pids-limit 512 --cpus 2 --network none --user " + user +
				" --cap-drop ALL --security-opt no-new-privileges -v " + box + "/lib:/src -w /src alpine sh -c 'HOME=/tmp ls'",
		} {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	})
	t.Run("[G02] the container is named for the job and limited; --user takes the job's gid", func(t *testing.T) {
		user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		safe := " --network none --cap-drop ALL --security-opt no-new-privileges -v " + box + "/lib:/src alpine sleep 86400"
		withUser := " --user " + user + safe
		name := " --name aj-j-x"
		limits := " --memory 2g --pids-limit 512 --cpus 2"
		for _, c := range []string{
			"docker run --rm" + limits + withUser,
			"docker run --rm --name other" + limits + withUser,
			"docker run --rm --name aj-otherjob-x" + limits + withUser,
			"docker run --rm --name aj-j" + limits + withUser,
			"docker run --rm --name aj-jj-x" + limits + withUser,
			"docker run --rm" + name + " --pids-limit 512 --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit 512" + withUser,
			"docker run --rm" + name + " --memory 1t --pids-limit 512 --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit -1 --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit 0 --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit 100000 --cpus 2" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit 512 --cpus 1000" + withUser,
			"docker run --rm" + name + " --memory 2g --pids-limit 512 --cpus 0" + withUser,
			"docker run --rm" + name + " --memory 2g --memory-swap -1 --pids-limit 512 --cpus 2" + withUser,
			"docker run --rm" + name + limits + " --user " + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()+1) + safe,
		} {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
		noID := cfg
		noID.JobID = ""
		if r := Check("docker run --rm"+name+limits+withUser, noID); r == "" {
			t.Error("docker allowed without a job id")
		}
		if r := Check("docker run --rm"+name+limits+withUser, cfg); r != "" {
			t.Errorf("the named, limited form: refused: %s", r)
		}
		if !strings.Contains(sandboxRecipe, "--name aj-") || !strings.Contains(sandboxRecipe, "--pids-limit") {
			t.Errorf("the recipe the refusals show lacks the name or limits: %s", sandboxRecipe)
		}
	})
}
