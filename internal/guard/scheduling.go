package guard

import "strings"

// checkLinkScheduling refuses combinations whose link state cannot be captured
// by the sequential path snapshots: traps run later and pipeline/background
// commands can overlap any later extraction or link creation in the call.
func checkLinkScheduling(scripts []*script, _ string, cfg Config) string {
	if len(cfg.Protected) == 0 && cfg.StateDir == "" && len(cfg.NoRead) == 0 {
		return ""
	}
	links, concurrent, pathTrap := false, false, false
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			concurrent = concurrent || pl.piped || len(pl.cmds) > 1 || pl.after == "&"
			for _, c := range pl.cmds {
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				name, args := progName(argv[0]), argv[1:]
				if name == "trap" {
					endOptions := len(args) > 0 && args[0] == "--"
					if endOptions {
						args = args[1:]
					}
					if len(args) > 0 && args[0] != "" && args[0] != "-" && (endOptions || !strings.HasPrefix(args[0], "-")) {
						// Option-like bodies after -- may lack parsed nested code.
						pathTrap = pathTrap || len(c.nested) == 0 || strings.HasPrefix(args[0], "-") || scriptsTouchPaths(c.nested)
					}
				}
				links = links || archiveExtraction(name, args).extract || name == "ln" || name == "link"
				if name == "cp" || name == "mv" || name == "rsync" {
					preserve, _, _ := copyLinkMode(name, args)
					links = links || preserve
					if name == "cp" {
						for _, opt := range args {
							links = links || opt == "--symbolic-link" || strings.HasPrefix(opt, "-") && !strings.HasPrefix(opt, "--") && strings.ContainsRune(opt, 's')
						}
					}
				}
				for _, w := range c.words {
					links = links || strings.Contains(strings.ToLower(w), "symlink")
				}
			}
		}
	}
	if links && (concurrent || pathTrap) {
		return cannot("archive extraction or link creation combined with concurrent commands or a path-accessing trap; split them into separate calls")
	}
	return ""
}

// Only path-free shell builtins are exempt. Unknown commands may open paths,
// and substitutions in otherwise harmless output commands still run code.
func scriptsTouchPaths(scripts []*script) bool {
	for _, sc := range scripts {
		for _, pl := range sc.pipelines {
			for _, c := range pl.cmds {
				if len(c.inRedirs) > 0 || len(c.outRedirs) > 0 || c.stdin || len(c.subSources) > len(c.subScripts) || scriptsTouchPaths(c.subScripts) || scriptsTouchPaths(c.nested) {
					return true
				}
				argv := argvOf(c.words)
				if len(argv) == 0 {
					continue
				}
				switch progName(argv[0]) {
				case ":", "true", "false", "echo", "printf":
				default:
					return true
				}
			}
		}
	}
	return false
}
