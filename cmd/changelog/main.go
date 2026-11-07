// Command changelog assembles changelog.d fragments for a release.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var fragmentName = regexp.MustCompile(`^([1-9][0-9]*)\.([a-z]+)\.md$`)
var sectionHeading = regexp.MustCompile(`(?m)^### ([A-Za-z]+)\r?$`)
var unreleasedHeading = regexp.MustCompile(`(?m)^## \[Unreleased\][ \t]*\r?$`)

type fragment struct {
	name, issue, section string
	bullets              []string
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "changelog:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("changelog", flag.ContinueOnError)
	root := flags.String("root", ".", "repository directory")
	check := flags.Bool("check", false, "print assembled changelog without writing or deleting fragments")
	validate := flags.Bool("validate", false, "validate fragment names and bullet lines without writing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*check && *validate) {
		return fmt.Errorf("use --check or --validate, with no positional arguments")
	}
	path := filepath.Join(*root, "CHANGELOG.md")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("CHANGELOG.md must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	order, headings := sections(text)
	fragments, err := loadFragments(filepath.Join(*root, "changelog.d"), headings)
	if err != nil || *validate {
		return err
	}
	result := text
	if len(fragments) > 0 {
		result, err = assemble(text, fragments, order, headings)
		if err != nil {
			return err
		}
	}
	if *check {
		_, err := io.WriteString(out, result)
		return err
	}
	if len(fragments) == 0 {
		return nil
	}
	// Publish the full result before consuming any fragments. A failed validation or write
	// leaves every fragment intact. Run assembly once, in the release worktree.
	if err := replaceFile(path, []byte(result), info.Mode().Perm()); err != nil {
		return err
	}
	for _, f := range fragments {
		if err := os.Remove(filepath.Join(*root, "changelog.d", f.name)); err != nil {
			return fmt.Errorf("assembled CHANGELOG.md but could not remove %s: %w", f.name, err)
		}
	}
	return nil
}

func sections(text string) ([]string, map[string]string) {
	order := []string{"added", "changed", "deprecated", "removed", "fixed", "security"}
	headings := map[string]string{
		"added": "Added", "changed": "Changed", "deprecated": "Deprecated",
		"removed": "Removed", "fixed": "Fixed", "security": "Security",
	}
	var extra []string
	for _, match := range sectionHeading.FindAllStringSubmatch(text, -1) {
		key := strings.ToLower(match[1])
		if _, ok := headings[key]; !ok {
			headings[key] = match[1]
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(order, extra...), headings
}

func loadFragments(dir string, headings map[string]string) ([]fragment, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("changelog.d must be a directory, not a link or file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var fragments []fragment
	for _, entry := range entries {
		name := entry.Name()
		if name == "README.md" {
			continue
		}
		match := fragmentName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%s: expected <issue-number>.<section>.md", name)
		}
		if _, ok := headings[match[2]]; !ok {
			return nil, fmt.Errorf("%s: unknown section %q", name, match[2])
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: fragment must be a regular file", name)
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		for i, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			if !strings.HasPrefix(line, "- ") || strings.TrimSpace(strings.TrimPrefix(line, "- ")) == "" {
				return nil, fmt.Errorf("%s:%d: expected a non-empty markdown bullet '- text'", name, i+1)
			}
			lines[i] = line
		}
		fragments = append(fragments, fragment{name, match[1], match[2], lines})
	}
	// Compare decimal strings by length first, so issue 9 precedes 100 without
	// imposing a machine integer limit on valid issue numbers.
	sort.Slice(fragments, func(i, j int) bool {
		a, b := fragments[i], fragments[j]
		if len(a.issue) != len(b.issue) {
			return len(a.issue) < len(b.issue)
		}
		if a.issue != b.issue {
			return a.issue < b.issue
		}
		return a.name < b.name
	})
	return fragments, nil
}

func assemble(text string, fragments []fragment, order []string, headings map[string]string) (string, error) {
	matches := unreleasedHeading.FindAllStringIndex(text, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one ## [Unreleased] heading")
	}
	start := matches[0][1]
	end := len(text)
	if next := strings.Index(text[start:], "\n## "); next >= 0 {
		end = start + next + 1
	}
	bullets := make(map[string][]string)
	current := ""
	for _, line := range strings.Split(text[start:end], "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if match := sectionHeading.FindStringSubmatch(line); match != nil {
			current = strings.ToLower(match[1])
			continue
		}
		if current == "" || !strings.HasPrefix(line, "- ") {
			return "", fmt.Errorf("unsupported content in [Unreleased]: %q; expected section headings and bullets", line)
		}
		bullets[current] = append(bullets[current], line)
	}
	for _, f := range fragments {
		bullets[f.section] = append(bullets[f.section], f.bullets...)
	}
	var result strings.Builder
	result.WriteString(text[:start])
	result.WriteString("\n\n")
	for _, section := range order {
		if len(bullets[section]) == 0 {
			continue
		}
		fmt.Fprintf(&result, "### %s\n%s\n\n", headings[section], strings.Join(bullets[section], "\n"))
	}
	result.WriteString(text[end:])
	return result.String(), nil
}

func replaceFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".changelog-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
