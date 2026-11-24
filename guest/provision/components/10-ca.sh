# ca: a provision.sh component, sourced by it (lib.sh's helpers).

# --- ca: the extra CAs, one file each, replacing the last start's ---
# No jq here: a first start from a plain image has none yet. Without
# ca-certificates (the same), base's install trusts them.
if want ca; then
  dir=/usr/local/share/ca-certificates/mono-jig-extra
  rm -rf "$dir"
  if [[ -s $GUEST_CA ]]; then
    install -d -m 0755 "$dir"
    awk -v dir="$dir" '/-----BEGIN CERTIFICATE-----/ { n++; f = sprintf("%s/%d.crt", dir, n) }
      n { print > f } /-----END CERTIFICATE-----/ { close(f) }' "$GUEST_CA"
    log "extra CAs: $(find "$dir" -name '*.crt' | wc -l)"
  fi
  if command -v update-ca-certificates >/dev/null; then
    update-ca-certificates --fresh >/dev/null
  fi
fi
