package guard

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// linkOperations records locations where commands can preserve or extract links.
// install dereferences source links, so its existing destination checks suffice.
// Literal Python/Perl symlink calls already set makesLinks and are scanned as code.
func (s *pathScope) linkOperations(argv []string) {
	if s.over() {
		return
	}
	name, args := progName(argv[0]), argv[1:]
	preserve, recursive := name == "mv", name == "mv"
	for _, a := range args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "--") {
			switch a {
			case "--archive", "--no-dereference", "--links":
				preserve = true
			case "--dereference", "--copy-links":
				preserve = false
			}
			recursive = recursive || a == "--archive" || a == "--recursive"
		} else if strings.HasPrefix(a, "-") {
			for _, c := range a[1:] {
				if name == "cp" && strings.ContainsRune("aPdrR", c) || name == "rsync" && strings.ContainsRune("al", c) {
					preserve = true
				}
				if (name == "cp" || name == "rsync") && c == 'L' {
					preserve = false
				}
				recursive = recursive || strings.ContainsRune("arR", c)
			}
		}
	}
	if (name == "cp" || name == "mv" || name == "rsync") && preserve {
		ca := parseCopyArgs(name, args)
		for _, src := range ca.srcs {
			sp, ok := s.expandHome(src)
			if !ok {
				continue
			}
			for _, d := range ca.dests {
				dp, ok := s.expandHome(d)
				if !ok {
					continue
				}
				for _, cwd := range s.dirs {
					from, to := absoluteAt(sp, cwd), absoluteAt(dp, cwd)
					fi, err := os.Lstat(from)
					if err != nil {
						continue
					}
					contents := name == "rsync" && strings.HasSuffix(src, "/")
					if di, err := os.Stat(to); !ca.whole && !contents && (len(ca.srcs) > 1 || err == nil && di.IsDir()) {
						to = filepath.Join(to, filepath.Base(from))
					}
					if ca.parents {
						to = filepath.Join(absoluteAt(dp, cwd), strings.TrimLeft(sp, "/"))
					}
					if fi.Mode()&os.ModeSymlink != 0 {
						s.preserveLink(from, to)
					} else if recursive && fi.IsDir() {
						_ = filepath.WalkDir(from, func(p string, entry fs.DirEntry, err error) error {
							if s.over() {
								return fs.SkipAll
							}
							if err == nil && entry.Type()&os.ModeSymlink != 0 {
								rel, _ := filepath.Rel(from, p)
								s.preserveLink(p, filepath.Join(to, rel))
							}
							return nil
						})
					}
				}
			}
		}
	}
	if !extractsLinks(name, args) {
		return
	}
	dests := []string{"."}
	for i, a := range args {
		if ds := attachedDirs(name, a); len(ds) > 0 {
			dests = ds
		}
		if (a == "-C" || a == "-d" && name == "unzip" || a == "-D" && name == "cpio" || a == "--directory") && i+1 < len(args) {
			dests = []string{args[i+1]}
		}
		if strings.HasPrefix(a, "--directory=") {
			dests = []string{strings.TrimPrefix(a, "--directory=")}
		}
	}
	for _, d := range dests {
		if exp, ok := s.expandHome(d); ok {
			for _, cwd := range s.dirs {
				s.linkRoots = append(s.linkRoots, absoluteAt(exp, cwd))
			}
		}
	}
}

func absoluteAt(p, cwd string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

func extractsLinks(name string, args []string) bool {
	if name == "unzip" {
		return !slices.ContainsFunc(args, func(a string) bool {
			return strings.HasPrefix(a, "-") && strings.ContainsAny(a[1:], "lpvztTZ")
		})
	}
	if name != "tar" && name != "cpio" {
		return false
	}
	for i, a := range args {
		if a == "--extract" || name == "cpio" && a == "--pass-through" {
			return true
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		if strings.HasPrefix(a, "-") || name == "tar" && i == 0 {
			if name == "tar" && strings.ContainsRune(a, 'x') || name == "cpio" && strings.ContainsAny(a, "ip") {
				return true
			}
		}
	}
	return false
}

func (s *pathScope) preserveLink(from, to string) {
	if target, err := os.Readlink(from); err == nil {
		if s.futureLinks == nil {
			s.futureLinks = map[string]string{}
		}
		to = filepath.Join(resolvePath(filepath.Dir(to), "/"), filepath.Base(to))
		s.futureLinks[fold(to)] = target
	}
}

// futurePath walks before cleaning `..`: a relative link target is interpreted
// at its new location, which can differ from its source even within one call.
func (s *pathScope) futurePath(raw string) string {
	parts, cur, links := strings.Split(raw, "/"), "/", 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		if target, ok := s.futureLinks[fold(next)]; ok && links < 40 {
			links++
			if filepath.IsAbs(target) {
				cur = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
		} else {
			cur = resolvePath(next, "/")
		}
	}
	return cur
}

// extractedLinkPath limits uncertainty to an extraction destination and a path
// traversing a component that does not exist yet. Plain ../out outside it stays
// allowed; listing/testing an archive does not introduce uncertainty.
func (s *pathScope) extractedLinkPath(exp string) bool {
	if len(s.linkRoots) == 0 || strings.HasPrefix(exp, "-") {
		return false
	}
	for _, cwd := range s.dirs {
		raw := exp
		if !filepath.IsAbs(raw) {
			raw = cwd + "/" + raw
		}
		parts, cur := strings.Split(raw, "/"), "/"
		for i, part := range parts {
			if part == "" || part == "." {
				continue
			}
			next := filepath.Join(cur, part)
			if _, err := os.Lstat(next); err != nil && slices.ContainsFunc(s.linkRoots, func(root string) bool { return within(next, root) }) {
				rest := strings.Join(parts[i+1:], "/")
				if rest != "" && s.couldReach(rest, false) {
					return true
				}
			}
			cur = next
		}
	}
	return false
}
