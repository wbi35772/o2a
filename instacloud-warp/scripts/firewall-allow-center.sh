#!/usr/bin/env bash
# Allow only the three center gateways to reach the producer's SOCKS5 ports.
# Run on the WARP node as root: sudo ./scripts/firewall-allow-center.sh
set -Eeuo pipefail

PORTS="${PORTS:-1081:1096}"
CENTER_IPS="${CENTER_IPS:-13.229.234.225 18.212.22.159 3.79.179.114}"
COMMENT="${COMMENT:-warp-pool-producer}"
SOCKS_USER="${SOCKS_USER:-warpuser}"
SOCKS_PASS="${SOCKS_PASS:-WarpPass2026}"

if [[ ${EUID} -ne 0 ]]; then
  echo "error: run as root (sudo $0)" >&2
  exit 1
fi

expand_ports() {
  local spec="$1" first last
  if [[ "$spec" == *:* ]]; then
    first="${spec%%:*}"
    last="${spec##*:}"
    seq "$first" "$last"
  else
    printf '%s\n' "$spec"
  fi
}

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  echo "using ufw"
  for ip in $CENTER_IPS; do
    for port in $(expand_ports "$PORTS"); do
      ufw allow from "$ip" to any port "$port" proto tcp comment "$COMMENT" >/dev/null
    done
  done
  ufw status | grep -E "(${CENTER_IPS// /|})" || true
  exit 0
fi

if ! command -v iptables >/dev/null 2>&1; then
  echo "error: neither active ufw nor iptables found" >&2
  exit 1
fi

echo "using iptables"
for ip in $CENTER_IPS; do
  if iptables -C INPUT -p tcp -m multiport --dports "$PORTS" -s "$ip" -j ACCEPT 2>/dev/null; then
    echo "already allowed: $ip -> tcp/$PORTS"
  else
    iptables -I INPUT 1 -p tcp -m multiport --dports "$PORTS" -s "$ip" \
      -j ACCEPT -m comment --comment "$COMMENT"
    echo "allowed: $ip -> tcp/$PORTS"
  fi
done

echo
echo "current rules:"
iptables -S INPUT | grep -E 'warp-pool-producer|multiport' || true
echo
echo "note: persist rules across reboot with your distro's netfilter-persistent/iptables-save."
echo "verify from a center container:"
echo "  curl -sS -m 10 --socks5-hostname '${SOCKS_USER}:${SOCKS_PASS}@NODE_PUBLIC_IP:1081' https://opencode.ai/cdn-cgi/trace"
