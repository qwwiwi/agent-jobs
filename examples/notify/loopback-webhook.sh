#!/usr/bin/env bash
# Example notify.command for agent-jobs: hands a job-done line to a webhook on this machine, for
# an agent whose bridge (for example a chat bridge) can start a turn from a loopback HTTP call.
#
# config.json:
#   "notify": {"command": ["/path/to/loopback-webhook.sh", "<webhook URL>", "<token file>"]}
#
# The daemon runs it without a shell, once per job-done line, with the line on stdin and 10 seconds
# to finish. The line is data: it is sent as the request body and never becomes part of a command.
# The token is read from a file (mode 600) and reaches curl through a file descriptor, so it is
# neither in config.json nor in any process's arguments.
#
# What your webhook expects (path, auth header, body format) is its own contract: adapt the curl
# call below. This example sends the line as text/plain with a bearer token.
set -euo pipefail

url="${1:?usage: loopback-webhook.sh <webhook URL> <token file>}"
token_file="${2:?usage: loopback-webhook.sh <webhook URL> <token file>}"

# Loopback only: a job-done line carries job titles and must not leave the machine by a typo.
case "$url" in
  http://127.0.0.1:*|http://localhost:*|"http://[::1]:"*) ;;
  *) echo "loopback-webhook: refusing a URL that is not loopback http" >&2; exit 2 ;;
esac
if [[ ! -r "$token_file" ]]; then
  echo "loopback-webhook: token file is missing or unreadable" >&2
  exit 2
fi

token="$(<"$token_file")"
# printf is a shell builtin: the header goes to curl through a pipe, not through an argument.
# The body is this script's stdin, passed on as is. --noproxy keeps a proxy variable from sending a
# loopback call elsewhere; --max-time stays under the daemon's 10 s limit.
curl --silent --show-error --fail --noproxy '*' --max-time 8 \
  -H @<(printf 'Authorization: Bearer %s\n' "$token") \
  -H 'Content-Type: text/plain; charset=utf-8' \
  --data-binary @- \
  "$url" >/dev/null
echo "loopback-webhook: line delivered" >&2
