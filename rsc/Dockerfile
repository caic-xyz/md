# syntax=docker/dockerfile:1.6

# Copyright 2025 Marc-Antoine Ruel. All Rights Reserved. Use of this
# source code is governed by an Apache License, Version 2.0 that can be found
# in the LICENSE file.

ARG BASE_IMAGE=ghcr.io/caic-xyz/md
FROM ${BASE_IMAGE}

ARG BASE_IMAGE
ARG BASE_IMAGE_DIGEST=unknown
ARG CONTEXT_SHA=unknown
ARG CACHE_KEY=

ARG ENV_FILE

LABEL \
	org.opencontainers.image.licenses=Apache-2.0 \
	md.base_image="${BASE_IMAGE}" \
	md.base_digest="${BASE_IMAGE_DIGEST}" \
	md.context_sha="${CONTEXT_SHA}" \
	md.cache_key="${CACHE_KEY}"

# Specialize the SSH configuration using keys from the md-keys named context.
RUN rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
COPY --from=md-keys ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key
COPY --from=md-keys ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ed25519_key.pub
COPY --from=md-keys --chown=user:user authorized_keys /home/user/.ssh/authorized_keys
RUN chmod 0400 /home/user/.ssh/authorized_keys

# Copy the file setting environment variables.
COPY --chown=user:user ${ENV_FILE:+$ENV_FILE} ${ENV_FILE:+/home/user/.env}
