# Changelog fragments

Feature and fix branches add fragments here instead of editing the `[Unreleased]`
block in `CHANGELOG.md`. This keeps parallel branches from changing the same file.

- Name each file `<issue-number>.<section>.md`, for example `197.added.md`.
  The issue number is a positive decimal integer without leading zeros.
- Sections are `added`, `changed`, `deprecated`, `removed`, `fixed`, and `security`.
  Other section headings already present in `CHANGELOG.md` are also supported in
  lowercase, including `docs` for its `Docs` heading.
- Each file contains one or more markdown bullet lines, for example
  `- Add changelog fragments and validate PR titles (#197).`
  Several bullets are allowed. Do not add headings, blank lines, or continuation
  paragraphs. Write in English and include the issue reference in the bullet.
- Use one file per issue and section; append related bullets to that file.
  `README.md` is documentation and is excluded from validation and assembly.

From the repository root:

```bash
go run ./cmd/changelog --validate # check fragment names and content only
go run ./cmd/changelog --check    # print the full proposed changelog; change nothing
go run ./cmd/changelog            # update Unreleased, then delete consumed fragments
```

The assembler orders sections as Added, Changed, Deprecated, Removed, Fixed,
Security, then other existing headings alphabetically. Within a section, new
entries are sorted by numeric issue number; bullets within a file keep their
order. Existing Unreleased bullets precede new entries. Released history stays
byte-for-byte unchanged. An empty or absent fragment directory changes nothing.
Invalid input fails before any files are written or fragments consumed.

Only the release worktree runs assembly, once at a time. Inspect the preview and
diff, including deleted fragments, before committing. If fragment deletion fails
after the changelog was written, inspect and reconcile the remaining fragments
before retrying, to avoid duplicate entries. See [Releasing](../docs/RELEASING.md).
