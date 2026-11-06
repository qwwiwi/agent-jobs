# Issue #134: Serialized gate runner: head assert, exit marker, process-group kill, clean env

Heavy local gates (check.sh, pg suites, vite builds) caused OOM, orphan suites, a gate on the wrong HEAD and hung waiters (about 3-4 h in two weeks). One gate at a time per box, assert HEAD, always print exit=, kill the process group on timeout, drop TELEGRAM_* from env.

Source: speed and autonomy audit 2026-11-04.
