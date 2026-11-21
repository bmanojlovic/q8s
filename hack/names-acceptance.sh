#!/usr/bin/env bash
# names-acceptance.sh exercises the host consumers that must accept the
# underscore separator the names package uses. It is a developer script, not a
# unit test: it needs a disposable host with Podman 5, systemd and optionally
# Traefik. Never run it against a production Podman store: it creates and
# removes objects with names outside q8s's own namespace.
set -euo pipefail

container_name=example_ns_example_name
pod_name=example_ns_example_pod
volume_name=example_ns_example_volume
network_name=example_ns_example_network
socket_name=example_ns_example_name.socket
workdir=$(mktemp -d)
traefik_pid=

cleanup() {
  if [[ -n "${traefik_pid}" ]]; then
    kill "${traefik_pid}" >/dev/null 2>&1 || true
    wait "${traefik_pid}" 2>/dev/null || true
  fi
  podman rm -f "${container_name}" >/dev/null 2>&1 || true
  podman pod rm -f "${pod_name}" >/dev/null 2>&1 || true
  podman volume rm "${volume_name}" >/dev/null 2>&1 || true
  podman network rm "${network_name}" >/dev/null 2>&1 || true
  if [[ -e "${HOME}/.config/systemd/user/${socket_name}" ]]; then
    systemctl --user stop "${socket_name}" >/dev/null 2>&1 || true
    rm -f "${HOME}/.config/systemd/user/${socket_name}"
    systemctl --user daemon-reload >/dev/null 2>&1 || true
  fi
  rm -rf "${workdir}"
}
trap cleanup EXIT

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "skip: $1 is not installed" >&2
    return 1
  fi
}

echo "== podman names =="
require podman || exit 1
podman create --name "${container_name}" scratch
podman pod create --name "${pod_name}"
podman volume create "${volume_name}"
podman network create "${network_name}"
podman container exists "${container_name}"
podman pod exists "${pod_name}"
podman volume exists "${volume_name}"
podman network exists "${network_name}"

echo "== quadlet and systemd names =="
if command -v systemctl >/dev/null 2>&1 && command -v systemd-analyze >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  quadlet_dir="${workdir}/quadlets"
  mkdir -p "${quadlet_dir}"
  cat > "${quadlet_dir}/${container_name}.container" <<'EOF'
[Container]
Image=scratch
EOF

  # A temporary user unit directory keeps the check away from the real
  # ~/.config/containers/systemd tree. QUADLET_UNIT_DIRS is read by the
  # Quadlet generator when systemd runs daemon-reload.
  if QUADLET_UNIT_DIRS="${quadlet_dir}" systemctl --user daemon-reload; then
    service_file=$(systemctl --user show -p FragmentPath --value "${container_name}.service" 2>/dev/null || true)
    if [[ -n "${service_file}" && -f "${service_file}" ]]; then
      systemd-analyze verify "${service_file}" || true
    else
      echo "note: Quadlet did not generate ${container_name}.service; check QUADLET_UNIT_DIRS support" >&2
    fi
  fi

  user_unit_dir="${HOME}/.config/systemd/user"
  mkdir -p "${user_unit_dir}"
  cat > "${user_unit_dir}/${socket_name}" <<EOF
[Unit]
Description=names acceptance socket

[Socket]
ListenStream=%t/${container_name}.sock
FileDescriptorName=example_socket
Service=${container_name}.service

[Install]
WantedBy=sockets.target
EOF
  systemd-analyze verify "${user_unit_dir}/${socket_name}" || true
  systemctl --user daemon-reload || true
  systemctl --user stop "${socket_name}" >/dev/null 2>&1 || true
else
  echo "skip: systemd tools or a user manager are not available" >&2
fi

echo "== traefik file provider names =="
if command -v traefik >/dev/null 2>&1; then
  traefik_dir="${workdir}/traefik"
  mkdir -p "${traefik_dir}"
  cat > "${traefik_dir}/dynamic.yaml" <<'EOF'
http:
  routers:
    example_ns_example_name:
      rule: "Host(`example.com`)"
      service: example_ns_example_name
      middlewares:
        - example_ns_example_middleware
  services:
    example_ns_example_name:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:8080"
  middlewares:
    example_ns_example_middleware:
      headers:
        customRequestHeaders:
          X-Example: alice
EOF
  cat > "${workdir}/traefik.yaml" <<EOF
log:
  level: ERROR
entryPoints:
  web:
    address: "127.0.0.1:18080"
providers:
  file:
    directory: "${traefik_dir}"
    watch: false
EOF
  traefik --configFile="${workdir}/traefik.yaml" >"${workdir}/traefik.log" 2>&1 &
  traefik_pid=$!
  sleep 2
  if ! kill -0 "${traefik_pid}" >/dev/null 2>&1; then
    echo "traefik rejected the underscore configuration:" >&2
    cat "${workdir}/traefik.log" >&2
    exit 1
  fi
  echo "traefik accepted underscore router, service and middleware keys"
else
  echo "skip: traefik is not installed" >&2
fi

echo "names acceptance checks passed"
