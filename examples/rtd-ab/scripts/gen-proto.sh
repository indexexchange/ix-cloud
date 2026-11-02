#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Generates the Go bindings in pkg/pb/ from the IAB Tech Lab .proto files in proto/.
# pkg/pb/ is NOT committed: the Dockerfile runs this script in its build stage, and
# `make proto` runs it in a pinned Go container for local use and CI (Docker is the
# only host requirement). protoc is pinned and checksum-verified, so the output is
# reproducible. Nothing under pkg/pb/ is copied from anywhere: it is produced from
# the .proto sources.
set -eu

PROTOC_VERSION=33.4
case "$(uname -m)" in
  x86_64)  PROTOC_ARCH=x86_64;  PROTOC_SHA256=c0040ea9aef08fdeb2c74ca609b18d5fdbfc44ea0042fcfbfb38860d35f7dd66 ;;
  aarch64|arm64) PROTOC_ARCH=aarch_64; PROTOC_SHA256=15aa988f4a6090636525ec236a8e4b3aab41eef402751bd5bb2df6afd9b7b5a5 ;;
  *) echo "unsupported build architecture: $(uname -m)" >&2; exit 1 ;;
esac
PROTOC_GEN_GO_VERSION=v1.36.11
PROTOC_GEN_GO_GRPC_VERSION=v1.6.2
MODULE=github.com/indexexchange/rtd-ab

apt-get update -qq >/dev/null && apt-get install -y -qq unzip >/dev/null
curl -fsSL -o /tmp/protoc.zip \
  "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${PROTOC_ARCH}.zip"
echo "${PROTOC_SHA256}  /tmp/protoc.zip" | sha256sum -c -
unzip -q /tmp/protoc.zip -d /opt/protoc

GOBIN=/opt/bin go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
GOBIN=/opt/bin go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"
export PATH="/opt/bin:${PATH}"

# Start from a clean pkg/pb so removed or renamed messages never leave stale files.
rm -rf pkg/pb/artf pkg/pb/openrtb

# The .proto files carry no go_package, so each is mapped to this module's package
# here instead of editing the IAB sources.
MAP="Magenticrtbframework.proto=${MODULE}/pkg/pb/artf;artf,\
Magenticrtbframeworkservices.proto=${MODULE}/pkg/pb/artf;artf,\
Mcom/iabtechlab/openrtb/v2/openrtb.proto=${MODULE}/pkg/pb/openrtb;openrtb"

/opt/protoc/bin/protoc -I proto -I /opt/protoc/include \
  --go_out=. --go_opt="module=${MODULE},${MAP}" \
  --go-grpc_out=. --go-grpc_opt="module=${MODULE},${MAP}" \
  proto/agenticrtbframework.proto \
  proto/agenticrtbframeworkservices.proto \
  proto/com/iabtechlab/openrtb/v2/openrtb.proto

chown -R "${HOST_UID:-0}:${HOST_GID:-0}" pkg/pb 2>/dev/null || true
echo "generated:"; find pkg/pb -name '*.go' | sort
