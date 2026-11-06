#!/bin/sh
# Prints the human-readable text of a `go test -json` stream: the Output field of every event, as plain
# `go test` would have written it. Reads stdin, writes stdout; a line that is not a JSON event goes
# through unchanged. check.sh runs the suite once with -json (the artifact manifest-check reads) and pipes
# the same stream through this filter, so the log still reads like a normal test run (#172).
#
# POSIX sh and awk only: no jq.
exec awk '
function hex(s,   i, n, c) {
  n = 0
  for (i = 1; i <= length(s); i++) {
    c = index("0123456789abcdef", tolower(substr(s, i, 1)))
    if (c == 0) return -1
    n = n * 16 + c - 1
  }
  return n
}
# The Output string of one event, decoded; "" when there is none.
function output(line,   p, rest, i, c, n, out) {
  p = index(line, "\"Output\":\"")
  if (p == 0) return ""
  rest = substr(line, p + 10)
  out = ""
  for (i = 1; i <= length(rest); i++) {
    c = substr(rest, i, 1)
    if (c == "\"") break
    if (c != "\\") { out = out c; continue }
    i++
    c = substr(rest, i, 1)
    if (c == "n") out = out "\n"
    else if (c == "t") out = out "\t"
    else if (c == "r") out = out "\r"
    else if (c == "b") out = out "\b"
    else if (c == "f") out = out "\f"
    else if (c == "u") {
      n = hex(substr(rest, i + 1, 4))
      if (n >= 32 && n < 127) out = out sprintf("%c", n)
      else out = out "\\u" substr(rest, i + 1, 4)
      i += 4
    } else out = out c
  }
  return out
}
/^\{/ { printf "%s", output($0); next }
{ print }
'
