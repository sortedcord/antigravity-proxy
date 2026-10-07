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

---

## Connecting with AI Gateways (e.g., Bifrost)

When integrating Antigravity Proxy with [Bifrost](https://getbifrost.ai/):

1. **Topology**:
   Place both containers on the same Docker network (e.g. `ai-stack`).
2. **Provider Configuration**:
   Configure Bifrost with its built-in `gemini` provider:
   ```json
   {
     "providers": {
       "antigravity-proxy": {
         "keys": [
           {
             "name": "antigravity-local",
             "value": "env.ANTIGRAVITY_PROXY_API_KEY",
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
3. **Important Configuration Details**:
   - The `base_url` must point to `http://antigravity-proxy:8080/v1beta` (including `/v1beta`).
   - Enable `allow_private_network` so Bifrost permits connections to container network IP addresses.
   - Set `ANTIGRAVITY_PROXY_API_KEY` in Bifrost's environment matching the proxy's `API_KEY`.
