# syntax=docker/dockerfile:1

# --- build stage -----------------------------------------------------------
FROM golang:1.26 AS build
WORKDIR /src

# Cache modules independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build metadata, passed from docker-compose / `docker build --build-arg`.
ARG VERSION=docker
ARG COMMIT=none
ARG BUILD_DATE=unknown

# CGO disabled -> a fully static binary that runs on the distroless "static"
# base with no libc. -trimpath keeps build paths out of the binary.
RUN CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w \
		-X github.com/typemore/typemore-server/internal/platform.Version=${VERSION} \
		-X github.com/typemore/typemore-server/internal/platform.Commit=${COMMIT} \
		-X github.com/typemore/typemore-server/internal/platform.BuildDate=${BUILD_DATE}" \
	-o /out/typemore-server ./cmd/server

# --- runtime stage ---------------------------------------------------------
# distroless/static-nonroot: no shell, no package manager, runs as a non-root
# user. Minimal attack surface for a single static binary.
FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/typemore-server /typemore-server

# Documented default; the process binds :8080 unless TYPEMORE_ADDR overrides it.
EXPOSE 8080
ENV TYPEMORE_ADDR=:8080

USER nonroot:nonroot
ENTRYPOINT ["/typemore-server"]
