#!/bin/sh
# sync.sh - materialize project branches listed in projects.tsv into ./projects/
set -eu
cd "$(dirname "$0")"
REPO_URL="${O2A_REPO_URL:-https://github.com/wbi35772/o2a.git}"
mkdir -p projects
tab=$(printf '\t')
while IFS="$tab" read -r name branch pin; do
  [ -n "$name" ] || continue
  case "$name" in \#*) continue ;; esac
  [ -n "$branch" ] || continue
  dir="projects/$name"
  if [ ! -d "$dir/.git" ]; then
    echo "clone  $name <- $branch"
    git clone --quiet --depth 1 --branch "$branch" "$REPO_URL" "$dir"
  elif [ "${O2A_SYNC_PULL:-0}" = "1" ]; then
    echo "pull   $name <- $branch"
    git -C "$dir" fetch --quiet --depth 1 origin "$branch"
    git -C "$dir" checkout --quiet -B deploy FETCH_HEAD
  fi
  if [ -n "$pin" ] && [ "$pin" != "-" ]; then
    if ! git -C "$dir" cat-file -e "$pin^{commit}" 2>/dev/null; then
      git -C "$dir" fetch --quiet --depth 1 origin "$pin" 2>/dev/null || true
    fi
    if git -C "$dir" cat-file -e "$pin^{commit}" 2>/dev/null; then
      git -C "$dir" -c advice.detachedHead=false checkout --quiet --detach "$pin"
    else
      echo "WARN   $name pin $pin unavailable; tip of $branch used" >&2
    fi
  fi
done < projects.tsv
echo "ready  $(ls -1 projects | wc -l | tr -d ' ') project(s)"
