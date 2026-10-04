# autobench in a container, with the generator it drives.
#
# k6 is not vendored by this repository, so the runtime image is the official k6
# image and the orchestrator is copied into it. That way the image has everything
# a benchmark needs and nothing to install, and the two versions cannot drift.
#
#   docker build -t autobench .
#   docker run --rm -v "$PWD:/bench" autobench capacity bench.yaml
#
# Pass --user when a result has to land in a mounted directory. The k6 image runs
# as uid 12345, so without it the container cannot write to a host-owned mount:
#
#   docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/bench" \
#     autobench capacity bench.yaml --json /bench/baseline.json
#
# Note on the `system` collector: a container's /proc is the host's, so inside a
# container it reports the machine running Docker rather than a cgroup view. That
# is the same host k6 runs on, which is what the collector is there to measure.

FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a source change does not refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/autobench ./cmd/autobench

# The runtime image is k6's, so the orchestrator and the generator ship together
# and their versions cannot drift apart.
FROM grafana/k6:2.1.0

COPY --from=build /out/autobench /usr/local/bin/autobench

# Benchmarks are mounted here.
WORKDIR /bench

# Compare takes two files rather than a config, so a bare `docker run` is only
# useful with a subcommand. Both are in the entrypoint.
ENTRYPOINT ["autobench"]
