#!/usr/bin/env bash
# Run the gated Podman checkpoint/restore tests inside a modern-podman
# container (invoked by ci.yml's test-integration-podman job, which docker-runs
# this privileged + --cgroupns=host so CRIU can use the runner's kernel).
#
# The hosted runner's own apt podman is unusable (24.04 has no criu; 22.04
# ships podman 3.4.4 whose runtime can't checkpoint and predates the libpod
# v5 API), so we bring podman 5.x + crun + criu via the container image and
# only need the runner for its kernel + Docker.
#
# Skips GREEN (exit 0 + ::warning::) if this runner can't actually
# checkpoint — e.g. the nested cgroup freezer is not permitted. Runs the
# tests for real (failing red) only once a checkpoint smoke test proves the
# environment is capable. Real C/R is also validated locally on podman 5.x +
# criu (OrbStack).
set -uo pipefail

dnf install -y -q criu iptables >/dev/null 2>&1 \
  || { echo "::warning::criu install failed in container — skipping C/R run"; exit 0; }
echo "stack: $(podman --version) / $(criu --version | head -1) / $(crun --version | head -1)"

criu check || { echo "::warning::criu check failed on this runner kernel — skipping C/R run"; exit 0; }

mkdir -p /etc/containers /run/podman
printf '[engine]\nevents_logger="file"\nruntime="crun"\n' > /etc/containers/containers.conf
podman system service --time=0 unix:///run/podman/podman.sock &
for _ in $(seq 1 30); do [ -S /run/podman/podman.sock ] && break; sleep 1; done
test -S /run/podman/podman.sock || { echo "::warning::podman service socket did not come up — skipping"; exit 0; }

# Capability smoke test: can this runner actually freeze + dump a container?
# Nested CRIU frequently can't ("Unable to freeze tasks: Operation not
# permitted"). If it can't, skip green with the real reason rather than fail.
podman run -d --name smoke docker.io/library/alpine:3.20 sleep 600 >/dev/null
sleep 2
if ! podman container checkpoint smoke >/tmp/ckpt.log 2>&1; then
  echo "::warning::this runner cannot checkpoint a container (likely cgroup freezer perms): $(tail -1 /tmp/ckpt.log) — skipping. Real C/R is validated locally on podman 5.x + criu."
  exit 0
fi
podman rm -f smoke >/dev/null 2>&1 || true
echo "checkpoint smoke passed — running the gated tests for real"

export PODMAN_SOCKET=unix:///run/podman/podman.sock
/w/podman.test -test.run TestIntegration -test.v -test.timeout 15m
/w/int.test -test.run '^TestPodman' -test.v -test.timeout 15m
