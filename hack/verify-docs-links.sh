#!/usr/bin/env bash
# verify-docs-links.sh fails when a Markdown file links to a repository path that
# does not exist. External links are not fetched: CI must not depend on the network
# or on someone else's site staying up.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

status=0
while IFS= read -r file; do
  dir="$(dirname "$file")"
  # Markdown inline links: [text](target). Skip anchors, absolute URLs, and mailto.
  while IFS= read -r target; do
    case "$target" in
      "" | \#* | http://* | https://* | mailto:*) continue ;;
    esac
    # Strip a fragment from a relative path.
    path="${target%%#*}"
    [ -z "$path" ] && continue
    if [[ "$path" == /* ]]; then
      candidate="$ROOT$path"
    else
      candidate="$dir/$path"
    fi
    if [ ! -e "$candidate" ]; then
      echo "FAIL: $file links to '$target' but $candidate does not exist" >&2
      status=1
    fi
  done < <(grep -oE '\]\([^)]+\)' "$file" | sed -E 's/^\]\((.*)\)$/\1/' || true)
done < <(git ls-files '*.md')

if [ "$status" -eq 0 ]; then
  echo "all relative Markdown links resolve"
fi
exit "$status"
