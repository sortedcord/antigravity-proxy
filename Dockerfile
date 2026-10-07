FROM golang:1.27.1-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/antigravity-proxy ./cmd/antigravity-proxy \
    && mkdir -p /home/app/.config/antigravity-proxy /home/app/.local/share/antigravity-proxy

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/antigravity-proxy /usr/local/bin/antigravity-proxy
COPY --from=build --chown=10001:10001 /home/app/ /home/app/

ENV HOME=/home/app \
    HOST=0.0.0.0 \
    PORT=8080 \
    ANTIGRAVITY_QUOTA_HISTORY_PATH=/home/app/.local/share/antigravity-proxy/usage.jsonl

USER 10001:10001
WORKDIR /home/app
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/antigravity-proxy"]
CMD ["serve"]
