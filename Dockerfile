# all2api — production image
# Multi-stage, cross-compilable (buildx --platform linux/amd64,linux/arm64),
# non-root, distroless final layer, web panel embedded via //go:embed.
#
# Build (local single-arch):
#   docker build -t all2api:dev .
# Build (multi-arch for GHCR):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t ghcr.io/jiangnan823/all2api:latest --push .
#
# Conventions assumed (see DEVOPS-M0-NOTES.md — change in one place if they shift):
#   module            all2api
#   entrypoint        ./cmd/gateway
#   binary            all2api
#   panel source      web-panel/            (vite build -> web-panel/dist; this file moves it)
#   panel embed dir   internal/web/dist     (backend: //go:embed all:dist)
#   listen            :7863
#   health endpoint   /healthz
#
# Requires backend to implement two things or this image will fail the health gate:
#   1) subcommand `all2api healthcheck`  -> exit 0 iff GET /healthz returns 200
#      (distroless has no shell and no wget, so HEALTHCHECK must be the app itself)
#   2) graceful SIGTERM shutdown within 10s (docker stop grace window)

# ----------------------------------------------------------------- web ------
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src

# Landing path convention (re-ruled by PM 2026-10-14, seq=870): the panel bundle lands in
# `web-panel/dist` — here the flattened `/src/dist` — and THIS stage's COPY below is the mover
# into the embed directory. Do not "fix" the path by pointing vite at ../internal/web/dist: the
# flatten would then resolve the outDir outside the stage and bake a placeholder silently.
#
# `emptyOutDir: false` on the fronted side keeps any `web-panel/dist/.gitkeep` alive, so the
# truth source for "was the panel built" is dist **minus .gitkeep**, and the old
# `if [ -z "$(ls -A dist)" ]` test was not that: an empty-but-present directory (or one holding
# only .gitkeep) produced a green placeholder bake. Now: a declared build script that produces
# no real files is a hard failure, and the placeholder branch only survives while no build script
# exists at all.
COPY web-panel/ ./

RUN set -eux; \
    if [ -f package.json ]; then \
      declared=$(node -e 'const fs=require("fs");const p=JSON.parse(fs.readFileSync("package.json","utf8"));console.log(p.scripts&&p.scripts.build?"true":"false")'); \
      npm ci; \
      npm run --if-present build; \
    else \
      declared=false; \
      echo "web-panel/package.json not found"; \
    fi; \
    real=$(find dist -type f ! -name .gitkeep 2>/dev/null | wc -l); \
    echo "panel build script declared=${declared}; real bundle files in dist (excluding .gitkeep)=${real}"; \
    if [ "${declared}" = "true" ] && [ "${real}" -eq 0 ]; then \
      echo "ERROR: package.json declares a build script but web-panel/dist holds no bundle — refusing to bake an image that would serve the placeholder panel"; \
      exit 1; \
    fi; \
    if [ "${real}" -eq 0 ]; then \
      echo "no panel bundle and no build script yet — embedding placeholder so //go:embed all:dist always compiles"; \
      mkdir -p dist && printf '<!doctype html><meta charset=utf-8><title>all2api panel pending</title>' > dist/index.html; \
    fi

# ---------------------------------------------------------------- build -----
# Patch-pinned to ci.yml's GO_VERSION: a container build on a different Go produces a
# different stdlib (and a different vulnerability set) than the gate that certifies it.
FROM golang:1.26.9-alpine AS build
WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata git

# go.mod only: the module is dependency-free today, so there is no go.sum to copy and
# `COPY go.mod go.sum ./` would abort the build with "file not found". When backend adds
# the first third-party dependency, commit go.sum AND add it to this COPY line.
COPY go.mod ./
RUN go mod download

COPY . .
# The mover (direction B, seq=870): the bundle staged at /src/dist becomes the embed content.
# The `web` stage has already failed the build if a build script was declared and this is empty,
# so this COPY can only ever carry a real bundle or an explicit placeholder.
COPY --from=web /src/dist /src/internal/web/dist

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO stays disabled on purpose: static binary, no libc dependency, distroless-safe,
# and the Windows exe remains a genuine single file. Re-enable only with review.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildDate=${BUILD_DATE}" \
      -o /out/all2api ./cmd/gateway

# ----------------------------------------------------------------- runtime --
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/all2api /app/all2api
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY config.example.json /app/config.example.json

# Runtime layout matches config: auth_dir=/app/auths, state_file=/app/data/state.json
WORKDIR /app
VOLUME ["/app/auths", "/app/data"]
EXPOSE 7863

ENV TZ=Asia/Shanghai \
    ALL2API_CONFIG=/app/config.json \
    GOMEMLIMIT=250MiB

USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/app/all2api", "healthcheck"]

ENTRYPOINT ["/app/all2api"]
CMD ["-config", "/app/config.json"]
