#!/usr/bin/env bash
# Starts one gpt-6.1-sol review per finished bench job; exits when all reviews are queued.
set -uo pipefail
B=<bench-dir>
REPO=<agent-jobs-repo>
BASE=$(git -C "$REPO" rev-parse origin/main)
RV="$B/reviews.tsv"; touch "$RV"
while :; do
  pending=0
  while IFS=$'\t' read -r lvl n tag id; do
    grep -q "^$id"$'\t' "$RV" && continue
    st=$(agent-jobs status --agent <agent> "$id" 2>/dev/null | awk '/^status:/{print $2; exit}'); st=${st:-unknown}
    case "$st" in
      queued|running|unknown) pending=$((pending+1)); continue ;;
    esac
    br="bench/$n-$tag"
    if [ "$st" != succeeded ] || ! git -C "$REPO" rev-parse -q --verify "refs/heads/$br" >/dev/null; then
      printf '%s\t%s\tno-review\t%s\n' "$id" "$st" "$(date -u +%FT%TZ)" >> "$RV"; echo "$(date -u +%T) $id $st, no branch $br"; continue
    fi
    brief="$B/review-brief-$n-$tag.md"
    { cat "$B/issue-$n.md"; cat <<EOF

---
## Review task (benchmark)
Review the change on branch \`$br\` (diff \`$BASE..HEAD\` in this worktree) as the fix for the issue above.
Judge correctness, whether the issue is fully fixed, regressions, test quality. Do not modify files.
End the report with exactly one line in this form, counts of findings by severity:
BENCH-VERDICT: critical=<n> high=<n> medium=<n> low=<n> issue_fixed=<yes|partly|no>
EOF
    } > "$brief"
    out=$(agent-jobs start --agent <agent> --kind review --model gpt-6.1-sol --reasoning-effort high --repo "$REPO" --base "$br" \
      --timeout-minutes 40 --title "BENCH-REVIEW $lvl #$n $tag" --brief-file "$brief" 2>&1)
    rid=$(printf '%s\n' "$out" | grep -v '^note:' | grep -oE '[0-9]{6}-[0-9]{6}-[0-9a-f]{4}' | head -1)
    printf '%s\t%s\t%s\t%s\n' "$id" "$st" "$rid" "$(date -u +%FT%TZ)" >> "$RV"
    echo "$(date -u +%T) review $rid for $id ($br)"
  done < "$B/manifest.tsv"
  [ "$pending" -eq 0 ] && { echo "all reviews queued"; exit 0; }
  sleep 120
done
