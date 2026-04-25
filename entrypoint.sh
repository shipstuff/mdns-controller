#!/bin/bash
set -euo pipefail

DEFAULT_DENY_INTERFACES="lo,docker0,cni0,flannel.1,kube-ipvs0,virbr0,zt*,tailscale0,wg0"

log() {
  echo "[$(date -Iseconds)] $*"
}

env_enabled() {
  local value="${1:-}"
  case "${value,,}" in
    ""|"true"|"1"|"yes"|"on"|"enable"|"enabled") return 0 ;;
    *) return 1 ;;
  esac
}

cleanup() {
  local exit_code=$?

  if [[ -n "${CONTROLLER_PID:-}" ]]; then
    kill "${CONTROLLER_PID}" 2>/dev/null || true
    wait "${CONTROLLER_PID}" 2>/dev/null || true
  fi

  if [[ -n "${AVAHI_PID:-}" ]]; then
    kill "${AVAHI_PID}" 2>/dev/null || true
    wait "${AVAHI_PID}" 2>/dev/null || true
  fi

  if [[ -n "${DBUS_PID:-}" ]]; then
    kill "${DBUS_PID}" 2>/dev/null || true
    wait "${DBUS_PID}" 2>/dev/null || true
  fi

  exit "${exit_code}"
}

trap cleanup EXIT INT TERM

host_avahi_available() {
  local host_socket="/host-run/dbus/system_bus_socket"
  [[ -S "${host_socket}" ]] || return 1

  DBUS_SYSTEM_BUS_ADDRESS="unix:path=${host_socket}" dbus-send \
    --system \
    --print-reply=literal \
    --dest=org.freedesktop.DBus \
    / org.freedesktop.DBus.NameHasOwner \
    string:org.freedesktop.Avahi 2>/dev/null | grep -q "true"
}

detect_interface() {
  if [[ -n "${MDNS_INTERFACE:-}" ]]; then
    echo "${MDNS_INTERFACE}"
    return
  fi

  ip -4 route list default | awk '{print $5; exit}'
}

detect_address() {
  local iface="$1"

  if [[ -n "${MDNS_ADDRESS:-}" ]]; then
    echo "${MDNS_ADDRESS}"
    return
  fi

  ip -4 -o addr show dev "${iface}" scope global | awk '{print $4}' | cut -d/ -f1 | head -n1
}

PRIMARY_INTERFACE="$(detect_interface)"
if [[ -z "${PRIMARY_INTERFACE}" ]]; then
  log "failed to detect a primary IPv4 interface"
  exit 1
fi

PRIMARY_ADDRESS="$(detect_address "${PRIMARY_INTERFACE}")"
if [[ -z "${PRIMARY_ADDRESS}" ]]; then
  log "failed to detect a primary IPv4 address on ${PRIMARY_INTERFACE}"
  exit 1
fi

mkdir -p /run/dbus /run/avahi-daemon /var/cache/avahi-daemon
rm -f /run/dbus/pid /run/dbus/system_bus_socket /run/avahi-daemon/pid
dbus-uuidgen --ensure

if id avahi >/dev/null 2>&1; then
  chown -R avahi:avahi /run/avahi-daemon /var/cache/avahi-daemon
fi

HOST_AVAHI_ENABLED=false
if env_enabled "${MDNS_ENABLE_HOST_AVAHI:-true}"; then
  HOST_AVAHI_ENABLED=true
fi

BUNDLED_AVAHI_ENABLED=false
if env_enabled "${MDNS_ENABLE_BUNDLED_AVAHI:-true}"; then
  BUNDLED_AVAHI_ENABLED=true
fi

DENY_INTERFACES="${DEFAULT_DENY_INTERFACES}"
if [[ -n "${MDNS_EXTRA_EXCLUDE_INTERFACES:-}" ]]; then
  DENY_INTERFACES="${DENY_INTERFACES},${MDNS_EXTRA_EXCLUDE_INTERFACES}"
fi

cat >/etc/avahi/avahi-daemon.conf <<EOF
[server]
host-name=$(hostname)
domain-name=local
use-ipv4=yes
use-ipv6=no
allow-interfaces=${PRIMARY_INTERFACE}
deny-interfaces=${DENY_INTERFACES}
ratelimit-interval-usec=1000000
ratelimit-burst=200

[publish]
publish-addresses=yes
publish-workstation=no
publish-domain=yes
publish-hinfo=no
publish-aaaa-on-ipv4=no
publish-a-on-ipv6=no

[reflector]
enable-reflector=no

[rlimits]
rlimit-core=0
EOF

if [[ "${HOST_AVAHI_ENABLED}" == "true" ]] && host_avahi_available; then
  export DBUS_SYSTEM_BUS_ADDRESS="unix:path=/host-run/dbus/system_bus_socket"
  log "detected host avahi on ${PRIMARY_INTERFACE}; using host dbus ${DBUS_SYSTEM_BUS_ADDRESS}"
else
  if [[ "${BUNDLED_AVAHI_ENABLED}" != "true" ]]; then
    log "host avahi unavailable or disabled and bundled avahi is disabled"
    exit 1
  fi
  export DBUS_SYSTEM_BUS_ADDRESS="unix:path=/run/dbus/system_bus_socket"
  log "starting bundled dbus-daemon"
  dbus-daemon --system --nofork --nopidfile --address="${DBUS_SYSTEM_BUS_ADDRESS}" &
  DBUS_PID=$!

  for _ in $(seq 1 20); do
    if [[ -S /run/dbus/system_bus_socket ]]; then
      break
    fi
    sleep 0.25
  done

  if [[ ! -S /run/dbus/system_bus_socket ]]; then
    log "bundled dbus socket did not become ready in time"
    exit 1
  fi

  log "starting bundled avahi-daemon on ${PRIMARY_INTERFACE} (${PRIMARY_ADDRESS})"
  avahi-daemon --no-chroot --debug &
  AVAHI_PID=$!

  for _ in $(seq 1 20); do
    if [[ -S /run/avahi-daemon/socket ]]; then
      break
    fi
    sleep 0.5
  done

  if [[ ! -S /run/avahi-daemon/socket ]]; then
    log "bundled avahi socket did not become ready in time"
    exit 1
  fi
fi

log "starting controller for ${PRIMARY_ADDRESS} via ${DBUS_SYSTEM_BUS_ADDRESS}"
/usr/local/bin/mdns-controller &
CONTROLLER_PID=$!

wait "${CONTROLLER_PID}"
