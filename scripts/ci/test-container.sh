#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

if ! command -v docker >/dev/null 2>&1; then
  if [ "${CI:-false}" = "true" ] || [ "${GITHUB_ACTIONS:-false}" = "true" ]; then
    echo "Error: Docker is required in CI environment but not found." >&2
    exit 1
  fi
  echo "Docker not available; skipping container smoke test in local environment."
  exit 0
fi

image_tag="antigravity-proxy:test"
container_name="antigravity-proxy-smoke-$$-$(date +%s)"

echo "Building container image..."
docker build -t "$image_tag" .

echo "Verifying container user is 10001:10001..."
user=$(docker image inspect -f '{{.Config.User}}' "$image_tag")
if [ "$user" != "10001:10001" ]; then
  echo "Expected container user 10001:10001, got $user" >&2
  exit 1
fi

echo "Starting container under hardened sandbox..."
trap 'docker logs "$container_name" 2>/dev/null || true; docker rm -f -v "$container_name" 2>/dev/null || true' EXIT

docker run -d \
  --name "$container_name" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=volume,dst=/home/app/.config/antigravity-proxy \
  --mount type=volume,dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8080:8080 \
  -e API_KEY=docker-smoke-inference \
  -e ADMIN_API_KEY=docker-smoke-admin \
  "$image_tag"

echo "Waiting for health endpoint..."
curl --fail --retry 20 --retry-delay 1 --retry-connrefused \
  http://127.0.0.1:8080/health > health.json

python3 - <<'PY'
import json
with open('health.json') as stream:
    health = json.load(stream)
assert health['status'] == 'ok', f"unexpected status: {health}"
assert health['credential_configured'] is False, f"credential_configured should be False"
PY
rm -f health.json

echo "Verifying authentication enforcement..."
unauth=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/admin/accounts)
if [ "$unauth" != "401" ]; then
  echo "Expected status 401 for unauthenticated request, got $unauth" >&2
  exit 1
fi

admin=$(curl -sS -o /dev/null -w '%{http_code}' -H 'x-api-key: docker-smoke-admin' http://127.0.0.1:8080/admin/accounts)
if [ "$admin" != "200" ]; then
  echo "Expected status 200 for valid admin key, got $admin" >&2
  exit 1
fi

echo "Stopping container..."
docker stop --time 30 "$container_name"
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$container_name")
if [ "$exit_code" != "0" ]; then
  echo "Container exited with non-zero code $exit_code" >&2
  exit 1
fi

echo "Container smoke test passed."
