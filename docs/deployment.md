# Deployment Guide

Antigravity Proxy can be deployed as a native binary, a standalone Docker container, or orchestrated via Docker Compose alongside AI gateways like Bifrost.

---

## Standalone Docker Container

The official Docker image is built from `scratch`, resulting in an ultra-lightweight, secure container with:
- Non-root execution under UID/GID `10001:10001`
- Static binary with embedded CA certificates
- No shell, package managers, or external utilities

### Running with a Mounted Configuration

The simplest and recommended approach is to run `./antigravity-proxy login` on the host first, then mount the resulting configuration into the container:

```sh
# Ensure storage directory exists
mkdir -p "$HOME/.local/share/antigravity-proxy"

docker run -d \
  --name antigravity-proxy \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --user "$(id -u):$(id -g)" \
  --mount type=bind,src="$HOME/.config/antigravity-proxy",dst=/home/app/.config/antigravity-proxy,readonly \
  --mount type=bind,src="$HOME/.local/share/antigravity-proxy",dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8080:8080 \
  -e API_KEY="your-secret-api-key" \
  antigravity-proxy
```

### Runtime Login in a Container

To configure or add accounts through `/admin/login-sessions`, use a writable private config directory instead of the `readonly` config mount above. Keep the container root filesystem read-only, but mount configuration and history as writable persistent directories. Both must belong to the container UID (10001 by default, or the explicit `--user` UID), and existing `config.json` must be mode `0600`.

```sh
mkdir -p "$HOME/.config/antigravity-proxy" "$HOME/.local/share/antigravity-proxy"
chmod 700 "$HOME/.config/antigravity-proxy"

docker run -d \
  --name antigravity-proxy \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --user "$(id -u):$(id -g)" \
  --mount type=bind,src="$HOME/.config/antigravity-proxy",dst=/home/app/.config/antigravity-proxy \
  --mount type=bind,src="$HOME/.local/share/antigravity-proxy",dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8080:8080 \
  -e API_KEY="your-secret-api-key" \
  antigravity-proxy
```

Do not supply token environment overrides for runtime login. The server remains up without a Google account; protected generation and model requests return `503` until login commits. Fetch the authorization URL with an API-key header, then submit `code`/`state` or the full loopback redirect URL as described in the [API reference](api.md#runtime-account-configuration). A browser outside the container normally cannot reach its loopback callback, so use manual submission or a tunnel. Remote management requests require HTTPS or an encrypted tunnel.

Account identity is persisted with credentials, and quota journals use hashed account namespaces. Preserve the entire history directory across restarts; old unlabelled journals are kept untouched.

### Running with Environment Variables

If you prefer to manage credentials strictly through environment variables or secret managers:

```sh
docker run -d \
  --name antigravity-proxy \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --mount type=volume,src=antigravity-history,dst=/home/app/.local/share/antigravity-proxy \
  -p 127.0.0.1:8080:8080 \
  -e API_KEY="your-secret-api-key" \
  -e ANTIGRAVITY_REFRESH_TOKEN="1//04..." \
  -e ANTIGRAVITY_OAUTH_CLIENT_ID="1071006060591-...apps.googleusercontent.com" \
  -e ANTIGRAVITY_OAUTH_CLIENT_SECRET="GOCSPX-..." \
  antigravity-proxy
```

---

## Production Docker Compose

Here is a hardened `compose.yaml` setup for deploying Antigravity Proxy alongside an internal Docker network (e.g., shared with Bifrost):

```yaml
services:
  antigravity-proxy:
    image: ghcr.io/sortedcord/antigravity-proxy:latest
    container_name: antigravity-proxy
    restart: unless-stopped
    read_only: true
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    environment:
      HOST: 0.0.0.0
      PORT: "8080"
      API_KEY: ${API_KEY:?API_KEY must be set}
      ANTIGRAVITY_QUOTA_HISTORY_PATH: /home/app/.local/share/antigravity-proxy/usage.jsonl
    volumes:
      - ./config:/home/app/.config/antigravity-proxy:ro
      - ./data:/home/app/.local/share/antigravity-proxy
    networks:
      - ai-stack
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

networks:
  ai-stack:
    external: true
```

*Note: Pre-create `./data` with permissions matching UID 10001 (`chown -R 10001:10001 ./data`) or your host UID.*

For runtime login with this Compose example, change the config volume to `./config:/home/app/.config/antigravity-proxy` (remove `:ro`) and give that directory the same private ownership as the data directory. `read_only: true` can remain enabled for the root filesystem.

---

## Connecting with AI Gateways (e.g., Bifrost)

When integrating Antigravity Proxy with [Bifrost](https://getbifrost.ai/):

1. **Topology**:
   Place both containers on the same Docker network (e.g. `ai-stack`).
2. **Multi-Account Provider Configuration**:
   Configure Bifrost with a single provider and multiple keys mapped to your Antigravity Proxy accounts:
   ```json
   {
     "providers": {
       "antigravity-proxy": {
         "keys": [
           {
             "name": "ag-personal",
             "value": "env.AG_KEY_PERSONAL",
             "models": ["*"],
             "weight": 1.0
           },
           {
             "name": "ag-work",
             "value": "env.AG_KEY_WORK",
             "models": ["*"],
             "weight": 1.0
           }
         ],
         "network_config": {
           "base_url": "http://antigravity-proxy:8080/v1beta",
           "allow_private_network": true
         },
         "custom_provider_config": {
           "base_provider_type": "gemini"
         }
       }
     }
   }
   ```
3. **Key Benefits**:
   - **Per-Account Metrics in Bifrost**: Bifrost tracks latency, token usage, and costs per key (`selected_key_name`), giving you isolated metrics for each Google account without running multiple proxy containers.
   - **Automatic Quota Failover**: When an account reaches its 5-hour or weekly Antigravity limit, the proxy returns `429 RESOURCE_EXHAUSTED` with retry metadata, prompting Bifrost to automatically switch to the next key.
   - **Single Traefik Router**: Only one backend URL and Traefik route is needed regardless of how many accounts you add.
4. **Important Configuration Details**:
   - The `base_url` must point to `http://antigravity-proxy:8080/v1beta` (including `/v1beta`).
   - Enable `allow_private_network` so Bifrost permits connections to container network IP addresses.
   - Set each account's API key (e.g., created via `antigravity-proxy keys create <account>` or `/admin/accounts/{id}/keys`) in Bifrost's environment.
