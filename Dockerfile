# ── Web build ─────────────────────────────────────────────────────────────────
FROM node:24-trixie-slim AS web-build
WORKDIR /build
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# ── Go build ──────────────────────────────────────────────────────────────────
FROM golang:1.24-trixie AS go-build
ARG VERSION=dev
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd/
COPY internal internal/
COPY --from=web-build /build/build internal/webui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tarea ./cmd/tarea

# ── Runtime ───────────────────────────────────────────────────────────────────
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --uid 10001 tarea \
    && mkdir -p /data/jobs \
    && chown -R tarea:tarea /data

COPY --from=go-build /out/tarea /usr/local/bin/tarea

# /data holds jobs/, state/, runs.jsonl and an optional .env (API keys).
# Mount it as a persistent volume.
VOLUME /data
WORKDIR /data
EXPOSE 8080

ENV TAREA_DATA=/data
USER tarea

# The panel has no auth: publish the port on loopback or put a proxy in front.
CMD ["tarea", "serve", "--addr", "0.0.0.0:8080"]
