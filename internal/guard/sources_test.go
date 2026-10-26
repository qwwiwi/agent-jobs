package guard

import (
	"os"
	"path/filepath"
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
		Sandbox: filepath.Join(jobDir, "sandbox")}
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
	t.Run("[G02] the sandbox recipe is allowed", func(t *testing.T) {
		for _, c := range []string{
			"docker run --rm --network none -v " + box + "/lib:/src -w /src node:22 sh -c 'npm ci && npm test'",
			"docker run --rm --network=none --volume=" + box + "/lib:/src:ro alpine ls -la /src",
			"docker run --rm -i --memory 2g --cpus 2 --pids-limit 512 --cap-drop ALL --security-opt no-new-privileges " +
				"--read-only --tmpfs /tmp -v " + box + "/lib:/src -w /src python:3.12 python -m pytest",
			"docker run --rm --network bridge -v " + box + "/lib:/src -w /src golang:1.23 go test ./...",
		} {
			if r := Check(c, cfg); r != "" {
				t.Errorf("%q: refused: %s", c, r)
			}
		}
	})
	t.Run("[G02] docker mounts and flags beyond a sandbox copy are refused", func(t *testing.T) {
		for _, c := range []string{
			// mounts
			"docker run --rm -v " + jobDir + "/sources/lib:/src alpine ls",
			"docker run --rm -v " + box + ":/src alpine ls",
			"docker run --rm -v " + box + "/home-link:/src alpine ls",
			"docker run --rm -v " + box + "/lib/../..:/src alpine ls",
			"docker run --rm -v /:/host alpine ls",
			"docker run --rm -v " + jobDir + "/work:/src alpine ls",
			"docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine ls",
			"docker run --rm -v " + box + "/lib:/src -v " + jobDir + "/work:/w alpine ls",
			"docker run --rm -v myvolume:/src alpine ls",
			"docker run --rm -v lib:/src alpine ls",
			"docker run --rm --mount type=bind,src=" + box + "/lib,dst=/src alpine ls",
			"docker run --rm --volumes-from other alpine ls",
			"docker run --rm -v " + box + "/lib:/src:z alpine ls",
			"docker run --rm alpine ls",
			// env and secrets
			"docker run --rm -e TOKEN=x -v " + box + "/lib:/src alpine ls",
			"docker run --rm --env TOKEN -v " + box + "/lib:/src alpine ls",
			"docker run --rm --env-file .env -v " + box + "/lib:/src alpine ls",
			"TOKEN=x docker run --rm -v " + box + "/lib:/src alpine ls",
			// privileges, host namespaces
			"docker run --rm --privileged -v " + box + "/lib:/src alpine ls",
			"docker run --rm --network host -v " + box + "/lib:/src alpine ls",
			"docker run --rm --net=host -v " + box + "/lib:/src alpine ls",
			"docker run --rm --network container:x -v " + box + "/lib:/src alpine ls",
			"docker run --rm --pid host -v " + box + "/lib:/src alpine ls",
			"docker run --rm --cap-add SYS_ADMIN -v " + box + "/lib:/src alpine ls",
			"docker run --rm --device /dev/kvm -v " + box + "/lib:/src alpine ls",
			"docker run --rm --security-opt seccomp=unconfined -v " + box + "/lib:/src alpine ls",
			"docker run --rm -p 8080:80 -v " + box + "/lib:/src alpine ls",
			// not throwaway, not the sole command, other subcommands
			"docker run -v " + box + "/lib:/src alpine ls",
			"docker run --rm -d -v " + box + "/lib:/src alpine sleep 100",
			"docker --context other run --rm -v " + box + "/lib:/src alpine ls",
			"docker exec x sh",
			"docker build ../sandbox/lib",
			"docker run --rm -v " + box + "/lib:/src alpine ls > out.txt",
			"cd .. && docker run --rm -v " + box + "/lib:/src alpine ls",
			"docker run --rm -v " + box + "/lib:/src alpine sh -c \"$(cat ~/.secrets/x)\"",
			"docker run --rm -v $HOME:/src alpine ls",
			"docker run --rm -v " + box + "/*:/src alpine ls",
			"timeout 60 docker run --rm -v " + box + "/lib:/src alpine ls",
			"podman run --rm -v " + box + "/lib:/src alpine ls",
		} {
			if r := Check(c, cfg); r == "" {
				t.Errorf("%q: allowed, want refused", c)
			}
		}
	})
}
