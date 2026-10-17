# Owner rules (always apply)

<!-- Template. Copy to the path configured as owner_rules_path (default ~/.agent-jobs/<agent>/owner-rules.md)
     and edit. Max 8 KB: a job refuses to start if this file is unreadable or larger. -->

- Commit messages in Russian, short and descriptive («Добавил проверку лимитов»).
- Code comments in English.
- Never commit secrets, `.env` files, keys or tokens.
- Do not add dependencies without a one-line justification in the report.
- Do not touch CI workflows; checks are local (`scripts/check.sh`).
