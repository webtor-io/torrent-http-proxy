#!/bin/sh
# Runs the blackhole tests (upstream_blackhole_linux_test.go) and the
# Linux-only sockopt test in a golang container with NET_ADMIN, where
# iptables can drop a test server's port.
#
#   services/testdata/blackhole.sh [go test -run regex]
#
# GO_TAG  golang image tag (default 1.26)
set -eu
src=$(cd "$(dirname "$0")/../.." && pwd)
tag=${GO_TAG:-1.26}
img=thp-blackhole:$tag
docker build -q -t "$img" - >/dev/null <<DOCKERFILE
FROM golang:$tag
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends iptables >/dev/null
DOCKERFILE
exec docker run --rm --cap-add NET_ADMIN -e GOFLAGS=-buildvcs=false \
	-v "$src:/src:ro" -v thp-gomod:/go/pkg/mod -v thp-gocache:/root/.cache/go-build -w /src "$img" \
	go test -tags blackhole -count=1 -v -timeout 10m -run "${1:-Blackhole|SilentUpstream|UpstreamDialer}" ./services/
