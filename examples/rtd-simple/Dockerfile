# rtd-simple: a no-op Index Cloud gRPC RTD service for onboarding tests.
#
# Container constraints from "Building your container":
#   - x86/amd64 architecture
#   - non-root execution as user 1000, group 1000
#   - read-only root filesystem (static binary, no runtime writes)
#   - no egress (this container makes no outbound calls)
#   - small image (FROM scratch; well under the 1 GiB limit)

# ---- build stage --------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
# Use the pinned module graph as-is (reproducible, preserves security-pinned
# indirect versions). Do NOT `go mod tidy` here: tidy re-runs MVS and would
# downgrade indirect deps bumped to patched releases.
RUN go mod download
# The protobuf Go bindings (pkg/pb) are not committed; generate them from the IAB
# .proto files with a pinned, checksum-verified protoc (see scripts/gen-proto.sh).
COPY proto/ ./proto/
COPY scripts/gen-proto.sh ./scripts/gen-proto.sh
RUN sh scripts/gen-proto.sh
COPY internal/ ./internal/
COPY cmd/ ./cmd/
RUN go test ./...   # fail the build if the no-op/contract logic is broken

ARG VERSION=dev
# TARGETOS/TARGETARCH come from BuildKit's build-time --platform flag, so the
# binary matches the image target (pass --platform=linux/amd64 for the x86 image
# Index Cloud requires).
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/rtd-simple ./cmd/server

# passwd/group for non-root uid/gid 1000 on scratch
RUN printf 'app:x:1000:1000::/:/sbin/nologin\n' > /out/passwd \
 && printf 'app:x:1000:\n' > /out/group
# MODEL_PATH must exist even though this container loads no model, so the mount
# point resolves. Ship it empty.
RUN mkdir -p /out/models

# ---- runtime stage ------------------------------------------------------------
# No hardcoded --platform: the target arch comes from the build-time --platform
# flag, which also set TARGETARCH above so the binary and image agree.
FROM scratch
# The IAB ARTF agent manifest is attached to this image as the "agent-manifest"
# label, injected at build time from manifest/manifest.json (see the Makefile's
# --label flag). It is image metadata, not a file in the container.
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group  /etc/group
COPY --from=build /out/rtd-simple /rtd-simple
COPY --from=build --chown=1000:1000 /out/models /models

# Ship the buildable source in the image so this sample can be built upon. It is
# read-only image content, not used at runtime (the entrypoint is the compiled
# binary); extract it with `docker cp <container>:/usr/src/rtd-simple ./src`.
# .dockerignore keeps internal-only material (SUMMARY.md, Documentation/) out.
# Licensed Apache-2.0 (see LICENSE).
COPY . /usr/src/rtd-simple

USER 1000:1000

ENV GRPC_PORT=50051 \
    SERVER_PORT=8080 \
    MODEL_PATH=/models

EXPOSE 50051 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/rtd-simple", "-healthcheck"]

ENTRYPOINT ["/rtd-simple"]
