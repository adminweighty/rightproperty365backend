# syntax=docker/dockerfile:1

# One multi-stage file for every binary. Pick the image with --target:
#
#   docker build -f deploy/docker/Dockerfile --target api     -t rp365-api .
#   docker build -f deploy/docker/Dockerfile --target worker  -t rp365-worker .
#   docker build -f deploy/docker/Dockerfile --target migrate -t rp365-migrate .
#
# Final images are distroless, static and run as a non-root user. They contain
# no shell and no secrets; configuration is injected through the environment.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
USER nonroot:nonroot
WORKDIR /app

FROM runtime AS api
COPY --from=build /out/api /app/api
EXPOSE 8080
ENTRYPOINT ["/app/api"]

FROM runtime AS worker
COPY --from=build /out/worker /app/worker
ENTRYPOINT ["/app/worker"]

FROM runtime AS adminctl
COPY --from=build /out/adminctl /app/adminctl
ENTRYPOINT ["/app/adminctl"]

FROM runtime AS migrate
COPY --from=build /out/migrate /app/migrate
ENTRYPOINT ["/app/migrate"]
CMD ["status"]
