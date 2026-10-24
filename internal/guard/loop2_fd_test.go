package guard

import (
	"testing"
)

// Fix loop 2, item 3: a descriptor opened by the shell on a directory holding a protected path (or on
// a path the guard cannot resolve) is a second name of it, and inline code reaching a descriptor by
// /proc/*/fd or /dev/fd changes the file behind it.
func TestFdThroughDirsExpansionsAndCode(t *testing.T) {
	gc, _, cfg := jobFolderSetup(t)
	t.Setenv("GIT_CONFIG_GLOBAL", gc)
	own := "'" + cfg.OwnDir + "'"
	refused(t, cfg,
		// (a) an input redirection of a directory holding a protected path
		"exec 9<"+own, "exec 9<..", "exec 9< ~/.agent-jobs", "exec 9<>..", "{ cat; } 9<..", "exec 9<"+own+"/",
		// (b) an input redirection of an expansion the guard cannot resolve
		`exec 9<"${GIT_CONFIG_GLOBAL:0}"`, `exec 9<"${GIT_CONFIG_GLOBAL#x}"`, `exec 9<"${!V}"`,
		`exec 9<"$(printf %s x)"`, "exec 9<`printf %s x`", `G=$GIT_CONFIG_GLOBAL; exec 9<"$G"`, `exec 9<$G`,
		`cat <"${GIT_CONFIG_GLOBAL:0:200}"`,
		// (c) inline code and here-doc programs naming a descriptor
		`python3 -c "import os; os.chmod('/proc/self/fd/9', 0o600)"`,
		`python3 -c "import os; os.chmod('/proc/self/fd/9/gitconfig', 0o600)"`,
		`python3 -c"import os; os.chmod('/proc/self/fd/9', 0o600)"`,
		`node -e "require('fs').chmodSync('/proc/self/fd/9/gitconfig', 0o600)"`,
		`ruby -e "File.chmod(0600, '/dev/fd/9')"`, `perl -e 'chmod 0600, "/proc/$$/fd/9"'`,
		`python3 -c "import os; os.chmod(f'/proc/{os.getpid()}/fd/9', 0o600)"`,
		"python3 - <<'EOF'\nimport os\nos.chmod('/proc/self/fd/9', 0o600)\nEOF",
		`python3 <<< "import os; os.chmod('/dev/fd/9', 0o600)"`,
		`exec 9<"${GIT_CONFIG_GLOBAL:0}"; python3 -c "import os; os.chmod('/proc/self/fd/9', 0o600)"`,
	)
	allowed(t, cfg, "cat < in.txt", `wc -l < "$f"`, `while read -r l; do echo "$l"; done < "$f"`,
		`sort < "${HOME}/in.txt"`, "exec 3>&1", `python3 -c "import sys; print(sys.stdin.read())" < in.txt`,
		"cat <<'EOF' > notes.md\nopen /proc/self/fd/9 is refused\nEOF", "cat /proc/self/status", "ls /dev/fd/")
}
