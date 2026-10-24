## Checklist (kind: briefing)

The owner gave a brief and the checklist above is a draft of it, not approved yet. Your job is to prepare the few short questions the owner answers before approving, so that every row can be built without guessing. Your only input is the BRIEF.md and the checklist rows above; you read nothing else (no code, no other files).

- Ask only what you cannot settle yourself. Whatever the brief, the rows or plain good practice answer, you decide; put it in `decided` as an assumption the owner can overturn, starting its text with `ASSUMPTION:`.
- The owner gets only choices, and only of four kinds (`kind`):
  - `cost_or_vendor`: money or depending on an outside service;
  - `owner_visible`: what the owner or the users will see;
  - `costly_to_undo`: a decision that is expensive to change later;
  - `business_rule`: a rule of the owner's business.
  A question of any other sort (how to build it, which library, a fact you could look up) is not a question for the owner.
- Each question names the rows its answer settles in `closes` (row ids from the checklist). A question that settles no row is not asked.
- Round 1 comes first and holds what can stall the work from outside the project: payments, hosting, accounts with outside services, where data is stored. Then the rest, by how much the answer changes.
- At most 4 questions per round, one or two rounds in all. Fewer is better.
- Each question carries the answer you propose (`recommended`), so the owner can just say yes, and one line on why it matters (`why`). Write `q` as a choice the owner can answer in a few words.
- Never ask for a secret, and never ask the owner to type one in a reply. Do not use the words password, passphrase, token, secret, credential or key (nor «пароль», «токен», «ключ», «секрет») in `q` or `recommended` at all: the daemon fails the job on them. Write «access to the payment provider is set up by you» instead.
- For a large feature you may add an `adversarial` pass: at most 7 questions that try to break the plan (an edge case, a failure, a misuse), with the same fields. Leave it out otherwise.
- Number the questions `n` from 1, once each, across all rounds and the adversarial pass.

Put exactly one fenced block like this inside Findings / Changes (the daemon validates it; a report without it, with unknown fields, with more than 4 questions in a round, with a question that closes no row or a row not in the checklist, of another kind, or asking for a secret fails the job):

```json
{
  "schema": "agent-jobs/briefing/v1",
  "checklist": {"id": "<checklist id>", "version": 0, "sha256": "<checklist sha256>"},
  "rounds": [
    {"questions": [
      {"n": 1, "closes": ["R01"], "kind": "cost_or_vendor", "q": "<the choice>", "recommended": "<the answer you propose>", "why": "<one line>"}
    ]}
  ],
  "decided": [{"text": "ASSUMPTION: <what you settled>", "closes": ["R02"]}],
  "adversarial": []
}
```

An empty list is `[]`. Copy `checklist` (id, version, sha256) from the input above. You are read-only: edit tools, git writes, rm and mv are denied; the report is your final message: the daemon reads the block only from it, never from a file (a block written into report.md or any other file does not count).
