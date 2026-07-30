# syntax=docker/dockerfile:1.9

# Production image for the TypeMore server.
#
# Two runtime targets share ONE build stage, so the server and the migration
# runner come out of a single module download and a single build cache instead
# of two independent passes over the same source:
#
#   docker build -t typemore-server  .                  # default target: server
#   docker build -t typemore-migrate --target migrate .
#
# The default (last) target is `server`, so an existing
# `docker compose build app` with no `target:` keeps building the server.

ARG GO_VERSION=1.26
# Pinned so rebuilding an old tag stays reproducible.
ARG BUSYBOX_TAG=1.37-uclibc

# --- dependencies ----------------------------------------------------------
# Split from the build so a source-only change never re-downloads modules:
# go.mod/go.sum move far less often than the code under them.
FROM golang:${GO_VERSION}-bookworm AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
	go mod download

# --- build -----------------------------------------------------------------
FROM deps AS build

# Build metadata, passed from docker-compose / `docker build --build-arg`.
ARG VERSION=docker
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Go build tags. `anticheat` compiles the replay REVIEW POLICY into the binary
# (docs/SELF_HOST.md) — its weights, threshold and combination rules are not
# present at all without it, and the server then reports `policy: none` at
# startup and on /healthz. Empty is the default so this image keeps behaving
# like `make build`; the production compose file sets `anticheat` explicitly.
ARG BUILD_TAGS=""

COPY . .

# CGO disabled -> a fully static binary that runs on the distroless "static"
# base with no libc. -trimpath keeps build paths out of the binary.
# The build-cache mount is what makes the second binary nearly free.
RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build \
	set -eu; \
	LDFLAGS="-s -w \
		-X github.com/typemore/typemore-server/internal/platform.Version=${VERSION} \
		-X github.com/typemore/typemore-server/internal/platform.Commit=${COMMIT} \
		-X github.com/typemore/typemore-server/internal/platform.BuildDate=${BUILD_DATE}"; \
	CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -trimpath \
		-ldflags "${LDFLAGS}" -o /out/typemore-server ./cmd/server; \
	CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -trimpath \
		-ldflags "${LDFLAGS}" -o /out/migrate ./cmd/migrate; \
	CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -trimpath \
		-ldflags "${LDFLAGS}" -o /out/quotectl ./cmd/quotectl

# --- healthcheck probe -----------------------------------------------------
# distroless has no shell and no curl, so HEALTHCHECK needs a binary to call.
# busybox:*-uclibc is fully static, so copying just /bin/wget out of it adds
# one ~1 MB file instead of a shell and a package manager.
FROM busybox:${BUSYBOX_TAG} AS probe

# --- migration runner ------------------------------------------------------
# One-shot: `migrate up` against TYPEMORE_DATABASE_URL, then exits. The server
# deliberately does not migrate on startup — bringing the schema up is a
# separate, ordered step (see the root docker-compose.yml). The image also
# carries /quotectl: the compose `quotes-import` one-shot runs it to publish
# the embedded quote corpus, without which quote mode serves an empty registry
# (DICTFIX_LOG.md, B-DICT-1).
# Declared BEFORE `server` so the default build target stays the server.
FROM gcr.io/distroless/static:nonroot AS migrate
LABEL org.opencontainers.image.title="typemore-migrate" \
	org.opencontainers.image.source="https://github.com/typemore/typemore-server" \
	org.opencontainers.image.description="Goose migration + quote-corpus runner for TypeMore"
COPY --from=build /out/migrate /migrate
COPY --from=build /out/quotectl /quotectl
USER nonroot:nonroot
ENTRYPOINT ["/migrate"]
CMD ["up"]

# --- server ----------------------------------------------------------------
# distroless/static:nonroot: no shell, no package manager, runs as a non-root
# user. Minimal attack surface for a single static binary.
FROM gcr.io/distroless/static:nonroot AS server
ARG VERSION=docker
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="typemore-server" \
	org.opencontainers.image.source="https://github.com/typemore/typemore-server" \
	org.opencontainers.image.description="TypeMore API + realtime server" \
	org.opencontainers.image.version="${VERSION}" \
	org.opencontainers.image.revision="${COMMIT}" \
	org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=probe /bin/wget /usr/bin/wget
COPY --from=build /out/typemore-server /typemore-server

# Documented default; the process binds :8080 unless TYPEMORE_ADDR overrides it.
EXPOSE 8080
ENV TYPEMORE_ADDR=:8080

# /readyz is liveness + a database ping, which is the condition that actually
# makes this container useful to the proxy in front of it. start-period covers
# pool warm-up; the replay worker does not gate readiness.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
	CMD ["/usr/bin/wget", "--quiet", "--tries=1", "--timeout=4", "--spider", "http://127.0.0.1:8080/readyz"]

USER nonroot:nonroot
ENTRYPOINT ["/typemore-server"]
