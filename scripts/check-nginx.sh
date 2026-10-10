#!/bin/bash
# Checks deploy/nginx.conf with `nginx -t` in a container running the
# server's nginx (1.24), so a typo in the vhost fails CI, not the reload on
# the server. The vhost names Let's Encrypt's files, so throwaway ones stand
# in for them: a self-signed certificate, empty SSL options, and DH
# parameters. Needs Docker and openssl.
#
#   bash scripts/check-nginx.sh
set -euo pipefail

IMAGE="nginx:1.24.0@sha256:f6daac2445b0ce70e64d77442ccf62839f3f1b4c24bf6746a857eff014e798c8"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

le="$work/letsencrypt"
mkdir -p "$le/live/cronwatch.dev"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=cronwatch.dev" \
    -keyout "$le/live/cronwatch.dev/privkey.pem" -out "$le/live/cronwatch.dev/fullchain.pem" 2>/dev/null
: >"$le/options-ssl-nginx.conf"
openssl dhparam -dsaparam -out "$le/ssl-dhparams.pem" 2048 2>/dev/null

# The official image includes conf.d/*.conf inside its http block, as
# sites-enabled is on the server.
docker run --rm \
    -v "$ROOT/deploy/nginx.conf:/etc/nginx/conf.d/cronwatch.conf:ro" \
    -v "$le:/etc/letsencrypt:ro" \
    "$IMAGE" nginx -t
