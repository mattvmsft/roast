#!/usr/bin/env bash
# Exercise the shipped image without credentials or a reachable object store.
set -euo pipefail

image=${1:?usage: test-broker-image.sh IMAGE}
container="roast-broker-test-$$"
trap 'docker logs "$container"; docker rm --force "$container" >/dev/null' EXIT
docker run --detach --name "$container" \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --publish 127.0.0.1::8080 \
  --env ROAST_BROKER_S3_ENDPOINT=http://127.0.0.1:1 \
  --env ROAST_BROKER_S3_BUCKET=smoke-test \
  --env ROAST_BROKER_S3_ACCESS_KEY=test \
  --env ROAST_BROKER_S3_SECRET_KEY=test \
  --env ROAST_BROKER_SIGNING_KEY=smoke-test-signing-key-not-a-secret \
  --env ROAST_BROKER_PRINCIPAL_HEADER=X-Test-Principal \
  "$image"
# Inspect the final image, not just the build-stage bundle. Distroless has no
# shell, so use Docker's archive API to read the supplemental assembly notice.
notice=$(docker cp "$container:/usr/share/licenses/roast/third-party/github.com/minio/md5-simd/NOTICE-Igneous-Systems-MIT.txt" - | tar -xOf -)
grep -Fq 'Copyright (c) 2018 Igneous Systems' <<<"$notice"
grep -Fq 'The above copyright notice and this permission notice shall be included in all' <<<"$notice"
grep -Fxq '  SOFTWARE.' <<<"$notice"
psl_notice=$(docker cp "$container:/usr/share/licenses/roast/third-party/golang.org/x/net/NOTICE-publicsuffix-MPL-2.0.txt" - | tar -xOf -)
grep -Fq 'https://github.com/publicsuffix/list/blob/' <<<"$psl_notice"
grep -Fxq 'Mozilla Public License Version 2.0' <<<"$psl_notice"
port=$(docker port "$container" 8080/tcp)
# The Docker port proxy can accept connections before the server is ready and
# return an empty reply rather than ECONNREFUSED. This GET is safe to retry.
curl --fail --silent --show-error --retry 10 --retry-all-errors \
  --max-time 5 --retry-delay 1 --retry-max-time 30 "http://$port/healthz"
docker exec "$container" /roast-broker healthcheck
