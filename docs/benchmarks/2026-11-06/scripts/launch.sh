#!/usr/bin/env bash
# Model benchmark launcher: same agent-jobs issue on several models.
set -euo pipefail
B=<bench-dir>
REPO=<agent-jobs-repo>
BASE=$(git -C "$REPO" rev-parse origin/main)
MAN="$B/manifest.tsv"
: > "$MAN"
declare -A LVL=([110]=S [145]=S [146]=S [154]=M [155]=M [114]=M [134]=L [119]=L [135]=L)
ORDER="110 154 134 145 155 119 146 114 135"
mk_brief() { # $1 issue, $2 model tag
  cat "$B/issue-$1.md"
  cat <<EOF

---
## Task (benchmark run, model tag: $2)
Implement the fix for the GitHub issue above in this repository (Go). Work only on what the issue asks.
- Read the code the issue points at before changing anything.
- Make exactly one tool call per step.
- Add or update tests that prove the fix; run \`go test\` for every package you touched and \`go vet ./...\`; paste the real exit codes in Evidence.
- Commit on a new branch \`bench/$1-$2\` with a clear English commit message. Do not push, do not open a PR, do not message anyone.
- If part of the issue needs an owner decision or is out of reach, do the rest and list it under "Not done / uncertain".
EOF
}
for n in $ORDER; do
  models="opus:high sonnet:high sonnet:medium glm:high"
  [ "${LVL[$n]}" = S ] && models="$models haiku:high"
  for me in $models; do
    m=${me%%:*}; e=${me##*:}; tag="$m-$e"
    case $m in
      glm) model="openrouter:z-ai/glm-5.3"; budget=1.5; tag="glm" ;;
      *) model="$m"; budget=50 ;;
    esac
    mk_brief "$n" "$tag" > "$B/brief-$n-$tag.md"
    out=$(agent-jobs start --agent <agent> --kind fix --size small --model "$model" --reasoning-effort "$e" \
      --repo "$REPO" --base "$BASE" --max-turns 200 --timeout-minutes 60 --max-budget-usd "$budget" \
      --title "BENCH ${LVL[$n]} #$n $tag" --brief-file "$B/brief-$n-$tag.md" 2>&1) || { echo "FAIL $n $tag: $out"; continue; }
    id=$(printf '%s\n' "$out" | grep -oE '[0-9]{6}-[0-9]{6}-[0-9a-f]{4}' | head -1)
    printf '%s\t%s\t%s\t%s\n' "${LVL[$n]}" "$n" "$tag" "$id" >> "$MAN"
    echo "queued ${LVL[$n]} #$n $tag $id"
  done
done
