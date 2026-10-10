#!/bin/sh
# After an edit: two things CI and the plan's operating rules require always to hold.
# gofmt must print nothing, and go.mod stays free of require lines (standard library only).
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
msg=""
if command -v gofmt >/dev/null 2>&1; then
  # Tracked and new files only: gofmt -l . would also walk .hrn/worktrees, where kept batch
  # attempts may hold unformatted code that is not this edit's.
  unformatted=$(git ls-files -co --exclude-standard -- '*.go' | xargs -r gofmt -l 2>/dev/null)
  if [ -n "$unformatted" ]; then
    msg=$(printf 'gofmt would change these files (run gofmt -w on them):\n%s\n' "$unformatted")
  fi
fi
if grep -qE '^[[:space:]]*require([[:space:]]|\(|$)' go.mod 2>/dev/null; then
  msg="${msg:+$msg
}go.mod has a require line: this module is standard library only, remove the dependency."
fi
if [ -n "$msg" ]; then
  printf '%s\n' "$msg" >&2
  exit 2
fi
exit 0
