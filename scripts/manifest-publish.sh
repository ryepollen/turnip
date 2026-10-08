#!/usr/bin/env bash
# manifest-publish.sh — variant A, step 1: publish the library MANIFEST.
#
# Mirrors the iCloud archive tree (orchestra and choral/{category}/{work}/...)
# to the VM as a skeleton of ZERO-BYTE stub files. The Go Catalog() scanner reads
# only filenames + work.yaml, never bytes, so the bot lists every work from a few
# kilobytes of stubs while the real gigabytes stay in iCloud. Audio only leaves
# iCloud at activation (later variant-A steps: trigger + direct Mac→R2 upload).
#
# Listing needs no file contents, so this NEVER materializes iCloud dataless
# files — find/ls see names without triggering downloads.
#
# This tool is idempotent and portable: point ARCHIVE at any work-folder tree.
# Runs on the Mac now; moves to the always-on Mac mini unchanged later.
#
# Usage:
#   ./manifest-publish.sh              # dry-run: build skeleton, print, don't send
#   ./manifest-publish.sh --push       # build + rsync to the VM
#   ARCHIVE=/tmp/demo ./manifest-publish.sh   # run against any tree
set -euo pipefail

# --- config (override via env) ----------------------------------------------
# Per-machine values (the VM address is an origin IP hidden behind Cloudflare,
# so it stays out of the public repo) live in gitignored scripts/env.local:
#   cp scripts/env.local.example scripts/env.local   # then fill in VM
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
[[ -f "$script_dir/env.local" ]] && . "$script_dir/env.local"
ARCHIVE="${ARCHIVE:-$HOME/Library/Mobile Documents/com~apple~CloudDocs/orchestra and choral}"
VM="${VM:?set VM (user@host) in scripts/env.local or the environment}"
# Live originals/ is now the target: variant-A steps 4+5 are done, so originals/
# holds only worthless 0-byte stubs + verbatim *.yaml — this mirror IS the catalog
# the VM scans. (Pre-split staging dir was turnip-manifest; override REMOTE_DIR to
# go back to a staging path if ever needed.) Files must end up owned by uid 1001 so
# the server (uid 1001) can read them, hence sudo rsync-path + a post-chown below.
REMOTE_DIR="${REMOTE_DIR:-/srv/var/audio/originals}"
CATEGORIES="${CATEGORIES:-books podcasts courses materials projects}"
# ----------------------------------------------------------------------------

PUSH=0
[[ "${1:-}" == "--push" ]] && PUSH=1

is_audio() { # $1 = filename → 0 if a supported audio extension
  case "$(printf '%s' "${1##*.}" | tr '[:upper:]' '[:lower:]')" in
    mp3|m4a|m4b|aac|ogg) return 0 ;;
    *) return 1 ;;
  esac
}

build="$(mktemp -d)"
trap 'rm -rf "$build"' EXIT

works=0 parts=0 yamls=0

for cat in $CATEGORIES; do
  src="$ARCHIVE/$cat"
  [[ -d "$src" ]] || continue

  # category channel config, carried verbatim (tiny text)
  if [[ -f "$src/feed.yaml" ]]; then
    mkdir -p "$build/$cat"; cp "$src/feed.yaml" "$build/$cat/feed.yaml"; yamls=$((yamls+1))
  fi

  # each immediate subdir is a work
  for workdir in "$src"/*/; do
    [[ -d "$workdir" ]] || continue
    work="$(basename "$workdir")"
    had_audio=0

    # zero-byte stub for every audio part at any depth (season subdirs included)
    while IFS= read -r -d '' f; do
      is_audio "$f" || continue
      rel="${f#"$src/"}"                     # {work}/[season/]part.ext
      mkdir -p "$build/$cat/$(dirname "$rel")"
      : > "$build/$cat/$rel"                  # empty stub
      parts=$((parts+1)); had_audio=1
    done < <(find "$workdir" -type f -print0)

    # per-work metadata, carried verbatim
    if [[ -f "$workdir/work.yaml" ]]; then
      mkdir -p "$build/$cat/$work"; cp "$workdir/work.yaml" "$build/$cat/$work/work.yaml"; yamls=$((yamls+1))
    fi

    [[ $had_audio -eq 1 ]] && works=$((works+1))
  done
done

echo "manifest: $works works, $parts stub parts, $yamls yaml files"
echo "source  : $ARCHIVE"

if [[ $PUSH -eq 0 ]]; then
  echo
  echo "DRY-RUN — would rsync to $VM:$REMOTE_DIR/  (pass --push to send)"
  echo "skeleton:"
  ( cd "$build" && find . -type f | LC_ALL=C sort | sed 's/^/  /' | head -80 )
  exit 0
fi

echo "pushing to $VM:$REMOTE_DIR/ ..."
# ADDITIVE by default: --size-only means every existing 0-byte stub (size 0 ==
# size 0) is skipped untouched, so only genuinely new works get written — no
# in-place churn of what the server is serving. Removing a work from the mirror
# is deliberate, not a side effect: pass PRUNE=1 to add --delete (destructive).
# -rlpD (no -o/-g) + sudo rsync-path writes as root; the chown normalizes new
# files to uid 1001 so the server can read them. DRY=1 previews the diff.
opts=(-rlpD --size-only --rsync-path="sudo rsync")
[[ "${PRUNE:-0}" == 1 ]] && opts+=(--delete)
[[ "${DRY:-0}" == 1 ]] && opts+=(-n -v)
rsync "${opts[@]}" "$build/" "$VM:$REMOTE_DIR/"
if [[ "${DRY:-0}" == 1 ]]; then
  echo "(dry-run: no chown; rerun without DRY=1 to apply)"
else
  ssh "$VM" "sudo chown -R 1001:1001 '$REMOTE_DIR'"
  echo "done."
fi
