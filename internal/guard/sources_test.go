package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sourcesSetup is a job with cloned sources: <jobdir>/sources/lib is the read-only clone, <jobdir>/sandbox
// holds the copies a docker container may mount, the call runs in <jobdir>/work.
func sourcesSetup(t *testing.T) (jobDir string, cfg Config) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CDPATH", "")
	jobDir = filepath.Join(home, ".agent-jobs", "thrall", "jobs", "j")
	for _, d := range []string{"work", "sources/lib/bin", "sandbox/lib", "tmp"} {
		if err := os.MkdirAll(filepath.Join(jobDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"sources/lib/install.sh", "sources/lib/bin/tool", "sources/lib/setup.py", "sandbox/lib/run.sh", "work/main.go"} {
		if err := os.WriteFile(filepath.Join(jobDir, f), []byte("x\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home), filepath.Join(jobDir, "sandbox", "home-link")); err != nil {
		t.Fatal(err)
	}
	cfg = Config{Dir: filepath.Join(jobDir, "work"), Sources: []string{filepath.Join(jobDir, "sources")},
		Sandbox: filepath.Join(jobDir, "sandbox"), JobID: "j"}
	return jobDir, cfg
}

func TestSourcesHostExecution(t *testing.T) {
	jobDir, cfg := sourcesSetup(t)
	src := filepath.Join(jobDir, "sources")
	t.Run("[G02] running cloned code on the host is refused", func(t *testing.T) {
		for _, c := range []string{
			src + "/lib/install.sh",
			"../sources/lib/bin/tool --help",
			"bash ../sources/lib/install.sh",
			"sh -c '../sources/lib/install.sh'",
			"python3 ../sources/lib/setup.py install",
			"cd ../sources/lib && make",
			"cd ../sources/lib; npm install",
			"npm --prefix ../sources/lib install",
			"pip install -e ../sources/lib",
			"pip install --editable=../sources/lib",
			"go build -C ../sources/lib ./...",
			"make -C ../sources/lib",
			"cargo build --manifest-path ../sources/lib/Cargo.toml",
			"source ../sources/lib/install.sh",
			". ../sources/lib/install.sh",
			"PATH=" + src + "/lib/bin:/usr/bin tool",
			"export PATH=" + src + "/lib/bin:$PATH",
			"LD_PRELOAD=../sources/lib/x.so ls",
			"cat ../sources/lib/install.sh | sh",
			"bash < ../sources/lib/install.sh",
			"find ../sources -name '*.sh' -exec sh {} ';'",
			"ls ../sources/lib/*.sh | xargs bash",
			"setsid ../sources/lib/install.sh",
			"timeout 60 ../sources/lib/install.sh",
			"env FOO=1 ../sources/lib/install.sh",
			"cp -r ../sources/lib ../tmp/lib",
			"rsync -a ../sources/lib/ ../tmp/lib/",
			"../sandbox/lib/run.sh",
			"bash ../sandbox/lib/run.sh",
			"cd ../sandbox/lib && npm test",
			"cd $X && make",
		} {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
			// The sources rule refuses it by itself, whatever an earlier rule says.
			if r := checkSourcesExec(parseAll(c), c, cfg); !strings.Contains(r, "never runs on the host") {
				t.Errorf("%q: want a sources refusal, got %q", c, r)
			}
		}
	})
	t.Run("[G02] reading the local copy stays allowed", func(t *testing.T) {
		for _, c := range []string{
			"cat ../sources/lib/install.sh",
			"grep -rn main ../sources/lib",
			"ls -la ../sources/lib",
			"head -50 ../sources/lib/setup.py",
			"find ../sources/lib -name '*.py'",
			"wc -l ../sources/lib/setup.py",
			"git -C ../sources/lib log --oneline -1",
			"cp -r ../sources/lib ../sandbox/lib2",
			"rm -rf ../sandbox/lib && cp -r ../sources/lib ../sandbox/lib",
			"go test ./...",
			"python3 -c 'print(1)'",
			"make test",
		} {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	})
	t.Run("[G02] writes into sources are refused", func(t *testing.T) {
		for _, c := range []string{
			"echo x > ../sources/lib/install.sh",
			"rm -rf ../sources/lib",
			"chmod -R u+w ../sources",
			"sed -i 's/a/b/' ../sources/lib/setup.py",
			"touch ../sources/lib/new",
			"cp main.go ../sources/lib/",
		} {
			if r := Check(c, cfg); !strings.Contains(r, "sources") {
				t.Errorf("%q: want a sources write refusal, got %q", c, r)
			}
		}
		if r := CheckFileWrite("Write", src+"/lib/setup.py", cfg); !strings.Contains(r, "sources") {
			t.Errorf("Write into sources: got %q", r)
		}
	})
	t.Run("[G02] a job without sources keeps its rules", func(t *testing.T) {
		plain := Config{Dir: cfg.Dir}
		if r := Check("bash ../sources/lib/install.sh", plain); r != "" {
			t.Errorf("no sources configured: refused: %s", r)
		}
		if r := Check("docker run --rm -v "+jobDir+"/sandbox/lib:/src alpine ls /src", plain); !strings.Contains(r, "outside the job's cgroup scope") {
			t.Errorf("no sandbox configured: docker must stay refused, got %q", r)
		}
	})
}

func TestSourcesDockerSandbox(t *testing.T) {
	jobDir, cfg := sourcesSetup(t)
	box := filepath.Join(jobDir, "sandbox")
	user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	// base is the required part of the sandbox form; each refused case adds one thing to it.
	base := "docker run --rm --name aj-j-1 --memory 2g --pids-limit 512 --cpus 2 --network none --user " + user +
		" --cap-drop ALL --security-opt no-new-privileges"
	mount := " -v " + box + "/lib:/src"
	lim := " --name aj-j-1 --memory 2g --pids-limit 512 --cpus 2"
	t.Run("[G02] the sandbox recipe is allowed", func(t *testing.T) {
		for _, c := range []string{
			base + mount + " -w /src node:22 sh -c 'HOME=/tmp npm ci && npm test'",
			"docker run --rm --name=aj-j-ls --memory=512m --pids-limit=64 --cpus=0.5 --network=none --user=" + user +
				" --cap-drop=ALL --security-opt=no-new-privileges --volume=" + box + "/lib:/src:ro alpine ls -la /src",
			base + " -i --read-only --tmpfs /tmp" + mount + " -w /src python:3.12 python -m pytest",
		} {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	})
	t.Run("[G02] docker mounts and flags beyond a sandbox copy are refused", func(t *testing.T) {
		for _, c := range []string{
			// mounts
			base + " -v " + jobDir + "/sources/lib:/src alpine ls",
			base + " -v " + box + ":/src alpine ls",
			base + " -v " + box + "/home-link:/src alpine ls",
			base + " -v " + box + "/lib/../..:/src alpine ls",
			base + " -v /:/host alpine ls",
			base + " -v " + jobDir + "/work:/src alpine ls",
			base + " -v /var/run/docker.sock:/var/run/docker.sock alpine ls",
			base + mount + " -v " + jobDir + "/work:/w alpine ls",
			base + " -v myvolume:/src alpine ls",
			base + " -v lib:/src alpine ls",
			base + " --mount type=bind,src=" + box + "/lib,dst=/src alpine ls",
			base + " --volumes-from other" + mount + " alpine ls",
			base + " -v " + box + "/lib:/src:z alpine ls",
			base + " alpine ls",
			// env and secrets
			base + " -e TOKEN=x" + mount + " alpine ls",
			base + " --env TOKEN" + mount + " alpine ls",
			base + " --env=TOKEN" + mount + " alpine ls",
			base + " --env-file .env" + mount + " alpine ls",
			"TOKEN=x " + base + mount + " alpine ls",
			// privileges, root, host namespaces and network
			base + " --privileged" + mount + " alpine ls",
			"docker run --rm" + lim + " --network host --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network bridge --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm --net=host --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network container:x --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network none --user 0:0 --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network none --user root --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network none --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network none --user " + user + " --security-opt no-new-privileges" + mount + " alpine ls",
			"docker run --rm" + lim + " --network none --user " + user + " --cap-drop ALL" + mount + " alpine ls",
			base + " --pid host" + mount + " alpine ls",
			base + " --cap-add SYS_ADMIN" + mount + " alpine ls",
			base + " --device /dev/kvm" + mount + " alpine ls",
			base + " --security-opt seccomp=unconfined" + mount + " alpine ls",
			base + " -p 8080:80" + mount + " alpine ls",
			// not throwaway, not the sole command, not the docker on PATH, other subcommands
			"docker run" + lim + " --network none --user " + user + " --cap-drop ALL --security-opt no-new-privileges" + mount + " alpine ls",
			base + " -d" + mount + " alpine sleep 100",
			"docker --context other" + strings.TrimPrefix(base, "docker") + mount + " alpine ls",
			"docker exec x sh",
			"docker build ../sandbox/lib",
			base + mount + " alpine ls > out.txt",
			"cd .. && " + base + mount + " alpine ls",
			base + mount + " alpine sh -c \"$(cat ~/.secrets/x)\"",
			base + " -v $HOME:/src alpine ls",
			base + " -v " + box + "/*:/src alpine ls",
			"timeout 60 " + base + mount + " alpine ls",
			"podman" + strings.TrimPrefix(base, "docker") + mount + " alpine ls",
			"./" + base + mount + " alpine ls",
			"../sandbox/lib/" + base + mount + " alpine ls",
			"../sources/lib/" + base + mount + " alpine ls",
			"DOCKER" + strings.TrimPrefix(base, "docker") + mount + " alpine ls",
		} {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
	})
}

// Fix loop 1 (review of d009a6c): cloned code reaches the host past a sandbox copy, git in a copy, and
// programs outside the runner list.
func TestSourcesHostExecutionLoop1(t *testing.T) {
	_, cfg := sourcesSetup(t)
	t.Run("[G02] copies out of the sandbox, git in a copy and non-reader programs are refused", func(t *testing.T) {
		for _, c := range []string{
			"cp -r ../sandbox/lib ../tmp/x",
			"cp -r ../sources/lib ../sandbox/x && cp -r ../sandbox/x ../tmp/x && bash ../tmp/x/install.sh",
			"mv ../sandbox/lib ../tmp/lib",
			"rsync -a ../sandbox/lib/ ../tmp/lib/",
			"ln -s ../sandbox/lib ../tmp/l",
			"git -C ../sandbox/lib diff",
			"cd ../sandbox/lib && git log -p",
			"git --git-dir=../sandbox/lib/.git log",
			"awk -f../sources/lib/x.awk data",
			"awk --file=../sources/lib/x.awk data",
			"sed -f../sources/lib/x.sed data",
			"sed e ../sources/lib/install.sh",
			"awk '{system($0)}' ../sources/lib/install.sh",
			"gawk -E ../sources/lib/x.awk",
			"gdb -batch -ex run ../sources/lib/bin/tool",
			"vim -S ../sources/lib/x.vim",
			"m4 ../sources/lib/x.m4",
			"qemu-x86_64 ../sources/lib/bin/tool",
			"sort --compress-program=../sources/lib/bin/tool ../sources/lib/setup.py",
			"rg --pre ../sources/lib/bin/tool main ../sources/lib",
			"rsync -e ../sources/lib/bin/tool a b:c",
			"tar -I ../sources/lib/bin/tool -cf ../tmp/x.tar ../tmp",
			"cd ../sources/lib && ./bin/tool",
			"cd ../sources/lib && tool",
			"python3 tools/analyze.py ../sources/lib",
		} {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
	})
	t.Run("[G02] readers fed from the sources stay allowed", func(t *testing.T) {
		for _, c := range []string{
			"find ../sources/lib -name '*.py' | xargs wc -l",
			"cat ../sources/lib/setup.py | wc -l",
			"rg -n main ../sources/lib",
			"cd ../sources/lib && grep -rn main .",
			"git -C ../sources/lib log --oneline -1",
			"chmod -R u+w ../sandbox/lib",
			"cp -R ../sources/lib ../sandbox/lib3",
			"diff -r ../sources/lib ../sandbox/lib",
		} {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	})
}
