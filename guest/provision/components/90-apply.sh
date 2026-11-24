# apply: a provision.sh component, sourced by it (lib.sh's helpers).

# --- apply: the settings the supervisor built (guest.Config), every start ---
if want apply; then
  log "settings ($GUEST_CONFIG)"
  cfg=$GUEST_CONFIG
  home="$(getent passwd "$SANDBOX_USER" | cut -d: -f6)"
  state=/var/lib/mono-jig
  install -d -m 0755 "$state"

  # Packages missing from an older image.
  missing=()
  while read -r p; do
    [[ -n $p ]] && ! dpkg -s "$p" >/dev/null 2>&1 && missing+=("$p")
  done < <(jq -r '.packages // [] | .[]' "$cfg")
  if (( ${#missing[@]} )); then
    log "packages: ${missing[*]}"
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends "${missing[@]}" >/dev/null
  fi

  # The environment: one block, replacing the per-tool ones older
  # provisioning wrote (80-84).
  for f in /etc/profile.d/8[0-4]-mono-jig-*.sh; do
    [[ -e $f ]] && client_env "$(basename "$f" .sh)"
  done
  env=()
  while read -r kv; do [[ -n $kv ]] && env+=("$kv"); done \
    < <(jq -r '.env // {} | to_entries[] | "\(.key)=\(.value)"' "$cfg")
  client_env 80-mono-jig "${env[@]}"
  echo 'unset GIT_ASKPASS SSH_ASKPASS' >>/etc/profile.d/80-mono-jig.sh

  # git: each key's values replace the guest's. The keys the last apply
  # set (recorded) and gitClear's go first, so a provider no longer served
  # leaves nothing.
  gitkeys=$state/guest-git
  while read -r k; do
    [[ -n $k ]] && { git config --system --unset-all "$k" 2>/dev/null || true; }
  done < <(jq -r '(.gitClear // []) + (.git // {} | keys) | .[]' "$cfg"; cat "$gitkeys" 2>/dev/null || true)
  while IFS=$'\t' read -r k v; do
    [[ -n $k ]] && git config --system --add "$k" "$v"
  done < <(jq -r '.git // {} | to_entries[] | .key as $k | .value[] | [$k, .] | @tsv' "$cfg")
  jq -r '.git // {} | keys | .[]' "$cfg" >"$gitkeys"

  # Files: written when absent or ours (a marker, if set, is the first
  # line). Those the last apply wrote and this one doesn't are removed, if
  # still ours. Merged JSON is never removed.
  ours() { [[ ! -e $1 || -z $2 ]] || [[ "$(head -n1 "$1")" == "$2" ]]; }
  mkdirs() { # <dir> <owner>: the missing directories, owned by owner
    [[ -d $1 ]] && return 0
    mkdirs "$(dirname "$1")" "$2"
    if [[ $2 == root ]]; then install -d -m 0755 "$1"; else install -d -m 0700 -o "$2" -g "$2" "$1"; fi
  }
  manifest=$state/guest-files
  written=$(mktemp)
  n=$(jq '.files // [] | length' "$cfg")
  for ((i = 0; i < n; i++)); do
    f=$(jq -c ".files[$i]" "$cfg")
    p=$(jq -r .path <<<"$f"); [[ $p == "~/"* ]] && p="$home/${p#\~/}"
    marker=$(jq -r '.marker // ""' <<<"$f")
    owner=$(jq -r '.owner // "root"' <<<"$f"); [[ $owner == user ]] && owner=$SANDBOX_USER
    mode=$(jq -r '.mode // "0644"' <<<"$f")
    link=$(jq -r '.link // ""' <<<"$f")
    if ! ours "$p" "$marker"; then
      log "$p exists and isn't ours: left alone"
      continue
    fi
    mkdirs "$(dirname "$p")" "$owner"
    if [[ -n $link ]]; then
      ln -sfn "$link" "$p"
    elif jq -e 'has("merge")' <<<"$f" >/dev/null; then
      [[ -s $p ]] || echo '{}' >"$p"
      jq --argjson m "$(jq -c .merge <<<"$f")" '. * $m' "$p" >"$p.tmp"
      mv "$p.tmp" "$p"
    else
      jq -j '.content // ""' <<<"$f" >"$p.tmp"
      mv "$p.tmp" "$p"
    fi
    if [[ ! -L $p ]]; then
      chown "$owner:$owner" "$p"
      chmod "$mode" "$p"
    fi
    jq -e 'has("merge")' <<<"$f" >/dev/null || printf '%s\t%s\n' "$p" "$marker" >>"$written"
  done
  if [[ -e $manifest ]]; then
    while IFS=$'\t' read -r p marker; do
      [[ -n $p ]] && [[ -e $p || -L $p ]] || continue
      cut -f1 "$written" | grep -qxF -- "$p" && continue
      if [[ -L $p ]] || ours "$p" "$marker"; then
        log "removing $p"
        rm -f "$p"
      fi
    done <"$manifest"
  fi
  mv "$written" "$manifest"
  chmod 0644 "$manifest"
fi
