# Issue #146: OpenRouter: file sources bypass providers.openrouter.deny_repos

Found while merging main (#144 sources) into the OpenRouter runner (#136).

`providers.openrouter.deny_repos` checks only the job's repository. With `job_start sources`, a file from a denied repository (or another job's report about a private repo) can be attached to an OpenRouter job and so reach a third-party provider.

Proposal: refuse `sources` for jobs whose runner is a third-party provider unless every source path is outside the denied repos and `job:` sources belong to jobs on allowed repos; or simpler, refuse sources on OpenRouter jobs until a policy exists.

Pilot workaround (owner chose the $20-capped pilot): OpenRouter jobs are started without sources, public repos only.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
