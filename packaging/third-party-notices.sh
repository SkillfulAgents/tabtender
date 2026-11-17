#!/usr/bin/env bash
# Writes the license notices of every module linked into tabtenderd to the
# given file. Binary distributions must carry them.
#
# Usage: packaging/third-party-notices.sh <output file>
set -euo pipefail

out="${1:?usage: third-party-notices.sh <output file>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

{
  echo "Tabtender includes the following third-party software."
  echo
  echo "================================================================================"
  echo "The Go programming language and standard library (LICENSE)"
  echo "================================================================================"
  # Kept in the repository: not every Go distribution ships the file.
  cat "$root/packaging/licenses/go.txt"
  echo
  # Every release platform, so one notice file serves all artifacts.
  for target in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64; do
    GOOS="${target%/*}" GOARCH="${target#*/}" go list -deps \
      -f '{{with .Module}}{{if not .Main}}{{.Path}}	{{.Version}}	{{.Dir}}{{end}}{{end}}' ./cmd/tabtenderd
  done | sort -u | while IFS=$'\t' read -r path version dir; do
    found=0
    for license in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE*; do
      [ -f "$license" ] || continue
      found=1
      echo "================================================================================"
      echo "$path $version ($(basename "$license"))"
      echo "================================================================================"
      cat "$license"
      echo
    done
    [ "$found" = 1 ] || { echo "no license file found for $path" >&2; exit 1; }
  done
} > "$out"
