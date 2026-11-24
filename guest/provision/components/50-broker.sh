# broker: a provision.sh component, sourced by it (lib.sh's helpers).

# --- broker: the broker's CA, trusted ---
# The providers' settings (URL rewrites, the tools' hosts and placeholder
# tokens) are in apply's settings, so they follow the broker's providers.
if want broker && [[ -n "$BROKER_CA_FILE" ]]; then
  install -m 0644 "$BROKER_CA_FILE" /usr/local/share/ca-certificates/mono-jig-broker.crt
  update-ca-certificates >/dev/null
fi
