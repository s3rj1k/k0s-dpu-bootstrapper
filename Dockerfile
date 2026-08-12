# syntax=docker/dockerfile:1
# SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
# SPDX-License-Identifier: Apache-2.0
ARG GO_VERSION=1.26

FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
WORKDIR /src

# Dependencies first so a source only change reuses this layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# The whole repository, because Go stamps the version from VCS state. A partial copy leaves
# tracked files missing, which reads as a modified work tree and collapses the version.
COPY . .
RUN git config --global --add safe.directory /src

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/bootstrapper ./cmd/bootstrapper

# Static distroless image with no shell and no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/bootstrapper /bootstrapper
USER 65532:65532
ENTRYPOINT ["/bootstrapper"]
