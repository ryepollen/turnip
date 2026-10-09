#!/usr/bin/env bash
# activate-agent.sh — variant A, steps 2–3: the Mac side of the activation trigger.
#
# The audio archive lives in iCloud (reachable only here); the bot runs on the VM.
# So the bot drops activation requests in the VM queue (requests/pending/*.json)
# and this agent — which can read iCloud and write R2 — drains them:
#
#   claim     atomic mv pending/{id}.json -> claimed/{id}.json  (double-claim impossible)
#   upload    materialize iCloud work parts -> R2 a/{secret}/{cat}/{work}/{rel}   (step 3)
#   report    mv claimed -> done/{id}.json      (VM watcher then applies state+feed, step 4)
#             on error: write failed/{id}.err + mv claimed -> failed/{id}.json
#
# One-shot by design: drains the queue once and exits, so launchd can run it on a
# StartInterval. Queue paths are ASCII (id = {nanos}-{op}-{cat}-{hash}); the work
# name (spaces/Cyrillic) lives only inside the JSON, parsed here, never in a path.
#
# Secrets (R2 creds + FEED_SECRET) live ONLY in a Mac-local env file (SECRETS_FILE,
# default ~/.turnip/publish.env), never fetched from the VM. FEED_SECRET is masked
# as *** in everything this script prints. See scripts/publish.env.example.
#
# Testable without a VM or R2:
#   VM=""            queue ops act on the local filesystem (no ssh, no sudo)
#   DRY_RUN=1        no rclone/brctl calls — just print the planned uploads
set -euo pipefail

VM="${VM-}"                     # set VM=user@host (env or launchd); explicit VM="" = local test mode
REMOTE_QUEUE="${REMOTE_QUEUE:-/srv/var/audio/requests}"
ARCHIVE="${ARCHIVE:-$HOME/Library/Mobile Documents/com~apple~CloudDocs/orchestra and choral}"
RUNAS="${RUNAS:-#1001}"        # VM queue is owned by uid 1001 (the bot); act AS that uid, not root
SECRETS_FILE="${SECRETS_FILE:-$HOME/.turnip/publish.env}"
DRY_RUN="${DRY_RUN:-0}"
MATERIALIZE_TIMEOUT="${MATERIALIZE_TIMEOUT:-1800}"   # seconds to wait for one iCloud part
AUDIO_EXT_FIND=( -iname '*.mp3' -o -iname '*.m4a' -o -iname '*.m4b' -o -iname '*.aac' -o -iname '*.ogg' )
FAIL_REASON=""

# ── queue plumbing (VM over ssh+sudo, or local when VM="") ───────────────────
# All callers pass ASCII-only paths, so no quoting of Unicode work names is needed.
qexec() {
  if [[ -z "$VM" ]]; then bash -c "$*"; return; fi
  # Run the whole command on the VM as uid 1001 (the queue owner), quoting-proof via base64.
  # Plain sudo (=root) would create files the uid-1001 finalize watcher can't move out of done/.
  local b64; b64=$(printf '%s' "$*" | base64 | tr -d '\n')
  ssh -o ConnectTimeout=15 "$VM" "printf %s '$b64' | base64 --decode | sudo -u '$RUNAS' bash"
}
json_field() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }
q_list_pending() { qexec "ls -1 $REMOTE_QUEUE/pending/*.json 2>/dev/null" | sed 's#.*/##; s#\.json$##'; }
q_read()  { qexec "cat $REMOTE_QUEUE/pending/$1.json"; }
q_claim() { qexec "mkdir -p $REMOTE_QUEUE/claimed && mv $REMOTE_QUEUE/pending/$1.json $REMOTE_QUEUE/claimed/$1.json"; }
q_done()  { qexec "mkdir -p $REMOTE_QUEUE/done && mv $REMOTE_QUEUE/claimed/$1.json $REMOTE_QUEUE/done/$1.json"; }
q_fail()  { qexec "mkdir -p $REMOTE_QUEUE/failed && printf %s $(printf '%q' "$2") > $REMOTE_QUEUE/failed/$1.err && mv $REMOTE_QUEUE/claimed/$1.json $REMOTE_QUEUE/failed/$1.json"; }

# ── secrets + R2 (rclone driven by env, so no `rclone config` step is needed) ─
load_secrets() { if [[ -f "$SECRETS_FILE" ]]; then set -a; . "$SECRETS_FILE"; set +a; fi; }

require_upload_env() {
  command -v rclone >/dev/null 2>&1 || { echo "ERROR: rclone not installed — run: brew install rclone"; exit 1; }
  local v missing=()
  for v in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET FEED_SECRET; do
    [[ -z "${!v:-}" ]] && missing+=("$v")
  done
  if (( ${#missing[@]} )); then
    echo "ERROR: missing ${missing[*]} in $SECRETS_FILE (see scripts/publish.env.example)"; exit 1
  fi
}

setup_rclone_env() {
  export RCLONE_CONFIG_R2_TYPE=s3
  export RCLONE_CONFIG_R2_PROVIDER=Cloudflare
  export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
  export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
  export RCLONE_CONFIG_R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
  export RCLONE_CONFIG_R2_REGION=auto
}

# mask_key hides FEED_SECRET in printed R2 keys (the secret is the whole privacy model)
mask_key() { local k="$1"; [[ -n "${FEED_SECRET:-}" ]] && k="${k//${FEED_SECRET}/***}"; printf '%s' "$k"; }

# mime_for mirrors app/publisher/feed.go audioMimeByExt exactly
mime_for() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    *.mp3) echo audio/mpeg ;;
    *.m4a|*.m4b) echo audio/mp4 ;;
    *.aac) echo audio/aac ;;
    *.ogg) echo audio/ogg ;;
    *) echo application/octet-stream ;;
  esac
}

# r2_stat_size echoes the object size in bytes, or nothing if the key is absent
r2_stat_size() {
  local out
  out="$(rclone lsjson --stat --timeout 30s --contimeout 15s "R2:$R2_BUCKET/$1" 2>/dev/null)" || return 1
  printf '%s' "$out" | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(d, list):
    d = d[0] if d else {}
print(d.get("Size", ""))' 2>/dev/null
}

# r2_put uploads one file under an exact key with the given Content-Type.
# Timeouts matter: without --timeout a stalled PUT (R2/network black-hole) hangs the
# whole one-shot run forever under -q. With them, a stall errors out → do_activate
# returns non-zero → q_fail → the bot reports the failure instead of silence.
r2_put() {
  rclone copyto "$1" "R2:$R2_BUCKET/$2" \
    --header-upload "Content-Type: $3" \
    --s3-no-check-bucket --s3-upload-cutoff 200M --s3-chunk-size 32M \
    --timeout 120s --contimeout 30s --retries 3 --low-level-retries 10 -q
}

# materialize forces a (possibly evicted, dataless) iCloud file fully to disk and
# waits until its allocated blocks reach ~the logical size, or times out.
materialize() {
  local f="$1" logical actual deadline
  [[ "$DRY_RUN" == 1 ]] && return 0
  brctl download "$f" >/dev/null 2>&1 || true
  logical="$(stat -f%z "$f")"
  [[ "${logical:-0}" -eq 0 ]] && return 0
  deadline=$(( $(date +%s) + MATERIALIZE_TIMEOUT ))
  while :; do
    actual=$(( $(du -k "$f" | cut -f1) * 1024 ))
    (( actual * 100 >= logical * 90 )) && return 0
    (( $(date +%s) >= deadline )) && return 1
    sleep 2
  done
}

# do_activate uploads every part of a work to R2 (skipping those already present),
# leaving originals untouched. Sets FAIL_REASON and returns non-zero on any failure;
# only a clean return authorizes the caller to mark the request done.
do_activate() {
  local cat="$1" work="$2" src="$3"
  local f rel key mime size i=0 up=0 skip=0
  local parts=()
  while IFS= read -r -d '' f; do parts+=("$f"); done \
    < <(find "$src" -type f \( "${AUDIO_EXT_FIND[@]}" \) -print0 | sort -z)
  local n=${#parts[@]}
  if (( n == 0 )); then FAIL_REASON="no audio parts in $cat/$work"; return 1; fi
  echo "  $n part(s) to publish"
  for f in "${parts[@]}"; do
    i=$((i+1))
    rel="${f#"$src"/}"
    key="a/${SECRET}/${cat}/${work}/${rel}"
    mime="$(mime_for "$rel")"
    if [[ "$DRY_RUN" != 1 ]] && size="$(r2_stat_size "$key")" && [[ -n "$size" && "$size" -gt 0 ]]; then
      echo "  [$i/$n] skip (already in R2): $(mask_key "$key")"
      skip=$((skip+1)); continue
    fi
    echo "  [$i/$n] → $(mask_key "$key")  ($mime)"
    if [[ "$DRY_RUN" == 1 ]]; then up=$((up+1)); continue; fi
    if ! materialize "$f"; then FAIL_REASON="iCloud download stalled: $rel"; return 1; fi
    if ! r2_put "$f" "$key" "$mime"; then FAIL_REASON="R2 upload failed: $rel"; return 1; fi
    up=$((up+1))
  done
  echo "  uploaded $up, skipped $skip of $n"
  return 0
}

# ── main ─────────────────────────────────────────────────────────────────────
load_secrets
if [[ "$DRY_RUN" != 1 ]]; then require_upload_env; setup_rclone_env; fi
SECRET="${FEED_SECRET:-***}"   # real secret drives R2 keys; *** only in a credential-less dry run

ids="$(q_list_pending || true)"
if [[ -z "${ids//[$'\t\r\n ']/}" ]]; then echo "no pending requests"; exit 0; fi

drained=0 failed=0
while IFS= read -r id; do
  [[ -z "$id" ]] && continue
  json="$(q_read "$id" 2>/dev/null)" || { echo "skip $id: cannot read request"; continue; }
  op="$(printf '%s' "$json"  | json_field op)"
  cat="$(printf '%s' "$json" | json_field category)"
  work="$(printf '%s' "$json"| json_field work)"
  echo "→ $id : $op $cat/$work"

  if ! q_claim "$id" 2>/dev/null; then echo "  already claimed elsewhere, skip"; continue; fi

  case "$op" in
    activate)
      src="$ARCHIVE/$cat/$work"
      if [[ ! -d "$src" ]]; then
        q_fail "$id" "work folder not found: $cat/$work"; echo "  FAILED: missing $src"; failed=$((failed+1)); continue
      fi
      FAIL_REASON=""
      if do_activate "$cat" "$work" "$src"; then
        q_done "$id"; echo "  done → VM watcher applies state+feed (step 4)"; drained=$((drained+1))
      else
        q_fail "$id" "${FAIL_REASON:-activation failed}"; echo "  FAILED: ${FAIL_REASON:-activation failed}"; failed=$((failed+1))
      fi
      ;;
    deactivate)
      # nothing to upload — the VM watcher evicts R2 + clears state+feed (step 4)
      q_done "$id"; echo "  done → VM watcher evicts + empties feed (step 4)"; drained=$((drained+1))
      ;;
    *)
      q_fail "$id" "unknown op: $op"; echo "  FAILED: unknown op $op"; failed=$((failed+1))
      ;;
  esac
done <<< "$ids"

echo "drained $drained, failed $failed"
