package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

type deliveredSkill struct {
	Name   string
	Target string
	Digest string
}

// skillTreeDigest includes every path, type, mode, symlink target and regular file's contents.
// Measure rejects special files and symlinks outside the skill or to directories before hashing.
func skillTreeDigest(dir string) (string, error) {
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
		return "", fmt.Errorf("skill directory moved or does not resolve")
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("skill directory is missing or no longer a directory")
	}
	if _, err := skills.Measure(dir); err != nil {
		return "", err
	}
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		var target, digest string
		if d.Type()&fs.ModeSymlink != 0 {
			target, err = os.Readlink(path)
		} else if fi.Mode().IsRegular() {
			var f *os.File
			f, err = os.Open(path)
			if err == nil {
				fileHash := sha256.New()
				_, err = io.Copy(fileHash, f)
				closeErr := f.Close()
				err = errors.Join(err, closeErr)
				digest = hex.EncodeToString(fileHash.Sum(nil))
			}
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(h).Encode([]any{filepath.ToSlash(rel), fi.Mode(), target, digest})
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func skillDeliveryManifest(work string, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	var entries []deliveredSkill
	for _, name := range names {
		target, err := os.Readlink(skills.LinkPath(work, name))
		if err != nil {
			return "", err
		}
		digest, err := skillTreeDigest(target)
		if err != nil {
			return "", fmt.Errorf("skill %q: %w", name, err)
		}
		entries = append(entries, deliveredSkill{Name: name, Target: target, Digest: digest})
	}
	b, err := json.Marshal(entries)
	return string(b), err
}

// checkedSkillLinks exempts only unchanged daemon deliveries recorded in the protected jobs DB.
// Even after cleanup removes a link, its target must still match: edits through a link must not
// disappear from the resume check when cleanup unlinks it. Legacy deliveries cannot be verified.
func checkedSkillLinks(j *store.Job, work string) ([]string, error) {
	if j.SkillsManifest == "" {
		if len(j.SkillsDelivered) > 0 {
			return nil, fmt.Errorf("delivered skills have no manifest: no verifiable baseline")
		}
		return nil, nil
	}
	var entries []deliveredSkill
	if err := json.Unmarshal([]byte(j.SkillsManifest), &entries); err != nil {
		return nil, fmt.Errorf("invalid skill delivery manifest: %w", err)
	}
	if len(entries) != len(j.SkillsDelivered) {
		return nil, fmt.Errorf("skill delivery manifest does not match delivered names")
	}
	seen := map[string]bool{}
	var left []string
	for _, e := range entries {
		if !skills.ValidName(e.Name) || seen[e.Name] || !slices.Contains(j.SkillsDelivered, e.Name) ||
			!filepath.IsAbs(e.Target) || filepath.Clean(e.Target) != e.Target {
			return nil, fmt.Errorf("invalid skill delivery manifest entry")
		}
		seen[e.Name] = true
		digest, err := skillTreeDigest(e.Target)
		if err != nil || digest != e.Digest {
			return nil, fmt.Errorf("delivered skill %q changed or cannot be verified", e.Name)
		}
		link := skills.LinkPath(work, e.Name)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, parent := range []string{".claude", skills.LinkDir} {
			if fi, err := os.Lstat(filepath.Join(work, parent)); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("delivered skill %q has a changed parent directory", e.Name)
			}
		}
		if target, err := os.Readlink(link); err != nil || target != e.Target {
			return nil, fmt.Errorf("delivered skill %q is no longer its recorded link", e.Name)
		}
		left = append(left, e.Name)
	}
	return left, nil
}
