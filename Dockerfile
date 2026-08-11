# Multi-arch by construction: the SQLite driver is pure Go, so CGO_ENABLED=0
# cross-compiles to amd64 and arm64 with no toolchain gymnastics. That is the
# whole reason §3 picked Go and a cgo-free driver.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src

# Dependency layer first, so source edits don't re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/interstellar ./cmd/interstellar

# Docker seeds a fresh named volume from the image's directory ownership. If
# /data arrives root-owned, the nonroot container cannot create its own database
# on first run -- so stage the directory with the right owner here.
RUN mkdir -p /out/data && chown 65532:65532 /out/data


FROM gcr.io/distroless/static-debian12:nonroot

# Static base ships CA certificates, which the HTTPS feed fetches need. The
# timezone database is embedded in the binary (time/tzdata), not installed here.
COPY --from=build /out/interstellar /interstellar
COPY --from=build --chown=65532:65532 /out/data /data

ENV PORT=8422 \
    DATA_DIR=/data

EXPOSE 8422
VOLUME /data
USER 65532:65532

ENTRYPOINT ["/interstellar"]
