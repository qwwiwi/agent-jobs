package guard

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

type linkState struct {
	links map[string]string
	roots []string
}

func (s *pathScope) useLinks(c *simpleCmd) {
	if at, ok := s.linksAt[c]; ok {
		s.futureLinks, s.linkRoots = at.links, at.roots
	}
}

var rsyncLongValues = map[string]bool{
	"--rsh": true, "--filter": true, "--exclude": true, "--include": true,
	"--exclude-from": true, "--include-from": true, "--files-from": true,
	"--temp-dir": true, "--backup-dir": true, "--suffix": true, "--remote-option": true,
}

// copyLinkMode respects option arity and keeps dereference policy independent of
// recursion: cp -Lr dereferences, cp -r defaults to preserving links, rsync -t
// preserves timestamps and does not consume a source operand.
func copyLinkMode(name string, args []string) (preserve, recursive, derefTop bool) {
	preserve, recursive = name == "mv", name == "mv"
	mode := ""
	vals := "tS"
	longs := copyLongVals
	if name == "rsync" {
		vals, longs = "eBfMT", rsyncLongValues
	}
	for i := 0; i < len(args); {
		if args[i] == "--" {
			break
		}
		if !strings.HasPrefix(args[i], "-") || args[i] == "-" {
			i++
			continue
		}
		opts, next := utilityOptions(args, i, vals, longs)
		i = next
		for _, opt := range opts {
			key := opt.name
			if name == "cp" && strings.HasPrefix(key, "--") {
				for _, full := range []string{"--archive", "--recursive", "--no-dereference", "--dereference"} {
					if len(key) > 2 && strings.HasPrefix(full, key) {
						key = full
						break
					}
				}
			}
			switch key {
			case "a", "--archive":
				recursive = true
				if name == "cp" {
					mode = "preserve"
				} else {
					preserve = true
				}
			case "r", "R", "--recursive":
				recursive = true
			case "P", "d", "--no-dereference":
				if name == "cp" {
					mode = "preserve"
				}
			case "l", "--links":
				if name == "rsync" {
					preserve = true
				}
			case "L", "--dereference", "--copy-links":
				mode = "dereference"
				preserve = false
			case "H":
				if name == "cp" {
					mode = "command-line"
				}
			}
		}
	}
	if name == "cp" {
		preserve = mode == "preserve" || recursive && mode != "dereference"
		derefTop = mode == "command-line"
	}
	return
}

// linkOperations models copies in command order, including sources created by
// earlier copies. An archive whose members cannot be read taints its extraction
// destination; known tar/zip symlinks are tracked at their exact destinations.
func (s *pathScope) linkOperations(argv []string) {
	if s.over() {
		return
	}
	name, args := progName(argv[0]), argv[1:]
	preserve, recursive, derefTop := copyLinkMode(name, args)
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
					contents := name == "rsync" && strings.HasSuffix(src, "/")
					if di, err := os.Stat(to); !ca.whole && !contents && (len(ca.srcs) > 1 || err == nil && di.IsDir()) {
						to = filepath.Join(to, filepath.Base(from))
					}
					if ca.parents {
						to = filepath.Join(absoluteAt(dp, cwd), strings.TrimLeft(sp, "/"))
					}
					if target, ok := s.linkTarget(from); ok && !derefTop && !contents {
						s.recordLink(to, target)
						continue
					}
					walkFrom := from
					if derefTop {
						walkFrom = s.futurePath(from)
					}
					fi, err := os.Stat(walkFrom)
					if err == nil && recursive && fi.IsDir() {
						_ = filepath.WalkDir(walkFrom, func(p string, entry fs.DirEntry, err error) error {
							if s.over() {
								return fs.SkipAll
							}
							if err == nil && entry.Type()&os.ModeSymlink != 0 {
								rel, _ := filepath.Rel(walkFrom, p)
								s.preserveLink(p, filepath.Join(to, rel))
							}
							return nil
						})
					}
					// A copied directory may contain links created only in this script.
					if recursive {
						for p, target := range maps.Clone(s.futureLinks) {
							if p != fold(from) && within(p, from) {
								rel, _ := filepath.Rel(fold(from), p)
								s.recordLink(filepath.Join(to, rel), target)
							}
						}
					}
				}
			}
		}
	}
	spec := archiveExtraction(name, args)
	if !spec.extract {
		return
	}
	for _, cwd := range s.dirs {
		dest := cwd
		for _, d := range spec.dirs {
			if exp, ok := s.expandHome(d); ok {
				dest = absoluteAt(exp, dest)
			} else {
				dest = cwd
			}
		}
		archive, ok := s.expandHome(spec.archive)
		if !ok || spec.unknown || archiveStdin(spec.archive) || s.archiveWritten(archive, cwd) || !s.archiveLinks(name, absoluteAt(archive, cwd), dest, spec.absolute) {
			s.linkRoots = append(s.linkRoots, dest)
		}
	}
}

// These names refer to the command's run-time stdin, not a check-time archive.
func archiveStdin(path string) bool {
	switch path {
	case "", "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0", "/proc/thread-self/fd/0":
		return true
	}
	return false
}

type extraction struct {
	extract      bool
	writeArchive bool // create/append/update changes the archive itself
	unknown      bool // archive semantics or member paths cannot be modelled exactly
	absolute     bool // tar -P/--absolute-names preserves leading slashes
	dirs         []string
	archive      string
}

func archiveExtraction(name string, args []string) (spec extraction) {
	if name != "tar" && name != "unzip" && name != "cpio" {
		return
	}
	vals, longs := "bITXfC", map[string]bool{"--file": true, "--directory": true, "--exclude": true, "--exclude-from": true, "--files-from": true, "--strip-components": true, "--transform": true, "--xform": true}
	if name == "unzip" {
		spec.extract = true
		vals = "dP"
	}
	if name == "cpio" {
		vals = "CEFHIMORD"
	}
	nonExtract := false
	apply := func(opt utilityOption) {
		if name == "tar" {
			// Only options whose effects on member paths and archive decoding are
			// understood may clear unknown-link provenance. In particular -i and
			// --ignore-zeros can expose members after Go's tar reader stops.
			switch opt.name {
			case "x", "--extract", "--get", "f", "--file", "C", "--directory",
				"P", "--absolute-names", "v", "--verbose", "z", "--gzip", "--gunzip", "--ungzip",
				"j", "--bzip2", "J", "--xz", "a", "--auto-compress", "p", "--same-permissions",
				"--preserve-permissions", "m", "--touch", "k", "--keep-old-files", "--no-same-owner",
				"--same-owner", "--numeric-owner", "--overwrite", "--exclude", "--exclude-from",
				"X", "t", "--list", "c", "--create", "r", "--append",
				"u", "--update", "A", "--concatenate", "--catenate":
			default:
				spec.unknown = true
			}
		}
		switch opt.name {
		case "P", "--absolute-names":
			if name == "tar" {
				spec.absolute = true
			}
		case "--strip-components", "--transform", "--xform", "--one-top-level":
			spec.unknown = true
		case "s":
			if name == "tar" {
				spec.unknown = true
			}
		case "j":
			if name == "unzip" {
				spec.unknown = true
			}
		case "x", "--extract", "--get":
			if name == "tar" {
				spec.extract = true
			}
		case "i", "p", "--pass-through":
			if name == "cpio" {
				spec.extract = true
			}
		case "t", "--list":
			if name == "tar" {
				nonExtract = true
			}
		case "c", "--create", "r", "--append", "u", "--update", "A", "--concatenate", "--catenate":
			if name == "tar" {
				nonExtract = true
				spec.writeArchive = true
			}
		case "f", "--file":
			if name == "tar" {
				spec.archive = opt.value
			}
		case "C", "--directory":
			if name == "tar" {
				spec.dirs = append(spec.dirs, opt.value)
			}
		case "d":
			if name == "unzip" {
				spec.dirs = []string{opt.value}
			}
		case "D":
			if name == "cpio" {
				spec.dirs = []string{opt.value}
			}
		}
	}
	for i := 0; i < len(args); {
		a := args[i]
		if a == "--" {
			if name == "unzip" && spec.archive == "" && i+1 < len(args) {
				spec.archive = args[i+1]
			}
			break
		}
		if name == "tar" && i == 0 && !strings.HasPrefix(a, "-") {
			i++
			for j := 0; j < len(a); j++ {
				opt := utilityOption{name: string(a[j])}
				if strings.IndexByte(vals, a[j]) >= 0 && i < len(args) {
					opt.value = args[i]
					i++
				}
				apply(opt)
			}
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if name == "unzip" && spec.archive == "" {
				spec.archive = a
			}
			i++
			continue
		}
		opts, next := utilityOptions(args, i, vals, longs)
		i = next
		for _, opt := range opts {
			if name == "unzip" && strings.ContainsAny(opt.name, "lpvztTZ") && len(opt.name) == 1 {
				spec.extract = false
			}
			apply(opt)
		}
	}
	if name == "tar" && spec.unknown && !nonExtract {
		spec.extract = true
	}
	return
}

// archiveLinks returns false for unreadable, unsupported or oversized archives.
// Partial knowledge is safe only together with unknown-link state for the root.
func (s *pathScope) archiveLinks(name, archive, dest string, absoluteNames bool) bool {
	if name != "tar" && name != "unzip" {
		return false
	}
	// O_NONBLOCK prevents a path replaced by a FIFO from hanging the guard.
	f, err := os.OpenFile(archive, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<20 {
		return false
	}
	if name == "unzip" {
		z, err := zip.NewReader(f, fi.Size())
		if err != nil {
			return false
		}
		for _, f := range z.File {
			if s.over() {
				return false
			}
			if f.Mode()&os.ModeSymlink == 0 {
				continue
			}
			r, err := f.Open()
			if err != nil {
				return false
			}
			data, err := io.ReadAll(io.LimitReader(r, maxWordBytes+1))
			r.Close()
			if err != nil || len(data) > maxWordBytes {
				return false
			}
			s.recordLink(absoluteAt(f.Name, dest), string(data))
		}
		return true
	}
	var r io.Reader = f
	if strings.HasSuffix(archive, ".gz") || strings.HasSuffix(archive, ".tgz") {
		g, err := gzip.NewReader(f)
		if err != nil {
			return false
		}
		defer g.Close()
		r = g
	}
	limited := &io.LimitedReader{R: r, N: 64 << 20}
	tr := tar.NewReader(limited)
	for {
		if s.over() {
			return false
		}
		h, err := tr.Next()
		if err == io.EOF {
			return limited.N > 0
		}
		if err != nil {
			return false
		}
		if h.Typeflag == tar.TypeLink {
			// Hard-link targets are relative to the archive root, not the member
			// directory. Retain unknown provenance rather than model them as symlinks.
			return false
		}
		if h.Typeflag == tar.TypeSymlink {
			member := h.Name
			if !absoluteNames {
				member = strings.TrimLeft(member, "/")
			}
			s.recordLink(absoluteAt(member, dest), h.Linkname)
		}
	}
}

func absoluteAt(p, cwd string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

func (s *pathScope) linkTarget(from string) (string, bool) {
	if target, ok := s.futureLinks[fold(from)]; ok {
		return target, true
	}
	target, err := os.Readlink(from)
	return target, err == nil
}

func (s *pathScope) preserveLink(from, to string) {
	if target, ok := s.linkTarget(from); ok {
		s.recordLink(to, target)
	}
}

func (s *pathScope) recordLink(to, target string) {
	if s.futureLinks == nil {
		s.futureLinks = map[string]string{}
	}
	to = filepath.Join(s.futurePath(filepath.Dir(to)), filepath.Base(to))
	s.futureLinks[fold(to)] = target
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
			cur = s.resolve(next)
		}
	}
	return cur
}

// extractedLinkPath includes existing components and direct file paths: extraction
// can replace either with a symlink. Uncertainty is confined to the destination.
func (s *pathScope) extractedLinkPath(exp string) bool {
	if len(s.linkRoots) == 0 || strings.HasPrefix(exp, "-") {
		return false
	}
	for _, cwd := range s.dirs {
		raw := exp
		if !filepath.IsAbs(raw) {
			raw = cwd + "/" + raw
		}
		cur := "/"
		// Check every component before cleaning ..: a link introduced under the
		// extraction root can redirect a path whose lexical result lies outside it.
		for _, part := range strings.Split(raw, "/") {
			if part == "" || part == "." {
				continue
			}
			if part == ".." {
				cur = filepath.Dir(cur)
				continue
			}
			cur = filepath.Join(cur, part)
			if slices.ContainsFunc(s.linkRoots, func(root string) bool { return cur != root && within(cur, root) }) {
				return true
			}
		}
	}
	return false
}
