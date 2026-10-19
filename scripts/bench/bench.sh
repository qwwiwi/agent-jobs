#!/usr/bin/env bash
# Run the benchmark: every task x arm, two repetitions, the second in reverse arm order.
set -euo pipefail
BENCH="${BENCH:?set BENCH}"
export BENCH AGENT_JOBS_HOME="${AGENT_JOBS_HOME:?set AGENT_JOBS_HOME}"
for rep in 1 2; do
  if [ "$rep" = 1 ]; then arms="fg bg job"; else arms="job bg fg"; fi
  for task in t1 t2 t3; do
    for arm in $arms; do
      if [ -f "$BENCH/runs/$arm-$task-$rep/result.json" ]; then echo "skip $arm-$task-$rep"; continue; fi
      echo "$(date '+%H:%M:%S') run $arm-$task-$rep"
      python3 "$BENCH/scripts/run.py" "$arm" "$task" "$rep" || echo "run $arm-$task-$rep failed"
    done
  done
done
echo "$(date '+%H:%M:%S') done"
