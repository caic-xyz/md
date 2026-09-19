# syntax=docker/dockerfile:1.6

# Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
# source code is governed by an Apache License, Version 2.0 that can be found
# in the LICENSE file.
#
# Dockerfile to create md-root: the stable root layer containing system
# packages and root-level tooling. Rebuilt infrequently; md-user builds
# on top of this.

FROM docker.io/debian:stable AS final
LABEL org.opencontainers.image.source=https://github.com/caic-xyz/md \
      org.opencontainers.image.licenses=Apache-2.0

ENV DEBIAN_FRONTEND=noninteractive
# BASH_ENV is sourced by bash for every non-interactive shell (e.g. "ssh host command").
# It sets up PATH for tools installed in user-specific directories (go, rust, nvm, bun, etc.).
ENV BASH_ENV=/etc/bash_env

COPY etc/ /etc/
COPY opt/ /opt/
COPY root/ /root/
COPY usr/ /usr/

# Make /root readable by all users (helps with diagnosing issues)
RUN chmod 755 /root && find /root -type f -exec chmod a+r {} + && find /root -type d -exec chmod a+rx {} +

# Setup measurement tool and version report file
RUN echo "| Step | Command | Duration |" > /var/log/build_timings.md && \
    echo "|---|---|---|" >> /var/log/build_timings.md && \
    chmod 666 /var/log/build_timings.md

# Install tools (as root)
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    measure_exec.sh "System Packages" /root/setup/1_packages.sh
RUN --mount=type=secret,id=github_token \
    if [ -f /run/secrets/github_token ]; then export GITHUB_TOKEN=$(cat /run/secrets/github_token); fi && \
    measure_exec.sh "Neovim" /root/setup/2_neovim.sh
RUN measure_exec.sh "Extrepo Packages" /root/setup/3_extrepo.sh
RUN measure_exec.sh "Create User" /root/setup/4_create_user.sh
RUN measure_exec.sh "KVM Setup" /root/setup/5_kvm.sh
RUN --mount=type=secret,id=github_token \
    if [ -f /run/secrets/github_token ]; then export GITHUB_TOKEN=$(cat /run/secrets/github_token); fi && \
    measure_exec.sh "radare2" /root/setup/6_radare2.sh
RUN measure_exec.sh "Podman" /root/setup/7_podman.sh

# Keep /etc/localtime a regular file. Debian points it at
# /usr/share/zoneinfo/Etc/UTC, and md bind-mounts the host /etc/localtime over
# /etc/localtime at container start; mounting over a symlink follows it and
# clobbers /usr/share/zoneinfo/Etc/UTC, so TZ=UTC resolves to the host zone. Run
# this last: any later tzdata configure would restore the symlink.
RUN rm -f /etc/localtime && cp /usr/share/zoneinfo/Etc/UTC /etc/localtime

# Stage to export root build timings
FROM scratch AS reports
COPY --from=final /var/log/build_timings.md /

FROM final
