# exitcheck's services (watch, analyze, bot) in one image. The runtime is Foundry's image because the checks fork the
# chain with anvil; the same version the tests and the README results use.
FROM golang:1.26.8-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# .git stays out of the build context, so VCS stamping is off; the version comes from -ldflags when given.
ARG VERSION=""
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X github.com/amirradjou/exitcheck/internal/buildinfo.Version=${VERSION}" \
    -o /out/exitcheck ./cmd/exitcheck

FROM ghcr.io/foundry-rs/foundry:v1.8.6
COPY --from=build /out/exitcheck /usr/local/bin/exitcheck
ENTRYPOINT ["exitcheck"]
