# kiro: a provision.sh component, sourced by it (lib.sh's helpers).

# --- kiro-cli: installed for every user in /usr/local/bin ---
# A pinned release, checked against its SHA-256 (from the release's
# manifest.json); no installer script is piped into a shell. Its settings,
# permissions and instructions are in apply's settings. Root owns the
# binary, so kiro-cli can't update itself; a new pin rebuilds the image.
if want kiro; then
  kiro_version=3.5.0
  case "$(uname -m)" in
    aarch64) kiro_arch=aarch64 kiro_sum=34444fee032944037282c762df4ff6929a51e827dd72d402cb7a8afbecb33e7f ;;
    x86_64) kiro_arch=x86_64 kiro_sum=98f016b746b342d8337a0e937cabecc381037e2e920ea0ffaae649e51ef59d4e ;;
    *) echo "kiro-cli: no build for $(uname -m)" >&2; exit 1 ;;
  esac
  if [ "$(/usr/local/bin/kiro-cli --version 2>/dev/null)" != "kiro-cli $kiro_version" ]; then
    log "kiro-cli $kiro_version"
    kiro_tmp=$(mktemp -d)
    curl -fsSL -o "$kiro_tmp/kirocli.tar.gz" \
      "https://prod.download.cli.kiro.dev/stable/$kiro_version/kirocli-$kiro_arch-linux.tar.gz"
    echo "$kiro_sum  $kiro_tmp/kirocli.tar.gz" | sha256sum -c --quiet -
    tar -xzf "$kiro_tmp/kirocli.tar.gz" -C "$kiro_tmp"
    KIRO_CLI_INSTALL_GLOBAL=1 sh "$kiro_tmp/kirocli/install.sh" >/dev/null
    rm -rf "$kiro_tmp"
  fi
  su - "$SANDBOX_USER" -c 'kiro-cli --version'
fi
