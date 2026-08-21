# Deployment

## Container

```bash
docker build -f deploy/Dockerfile -t openrealtime:1.0.0 .
docker run --rm -p 8080:8080 \
  -e OPENAI_API_KEY \
  openrealtime:1.0.0 serve -addr :8080
```

The image is distroless and runs as a non-root user. It contains the server
binary and the certificate roots and nothing else — no shell, no package
manager, no interpreter. A realtime server's job is to speak one protocol on
one port; everything beyond that is surface an attacker can use.

Configuration is environment and flags, never a baked-in file, so the same
image runs in every environment. `openrealtime serve -h` lists the flags, and
[../docs/operations.md](../docs/operations.md) covers what to set and why.

## Binaries

```bash
./scripts/build-release.sh dist
```

Produces static binaries for linux/amd64, linux/arm64, darwin/arm64,
darwin/amd64, and windows/amd64, with a `SHA256SUMS` file. The builds are
reproducible: the same source tree and Go toolchain produce byte-identical
binaries anywhere, which is what makes the checksums worth publishing. CI
verifies that property on every change rather than taking it on faith.

Every binary reports what it was built from:

```console
$ openrealtime version
openrealtime 1.0.0, revision 4f2c1a…, go go1.25.0
```

A tree with uncommitted changes is reported as `(modified)`, so a binary found
running somewhere can always be traced back to a source tree.

## Colocated model serving

[sglang-omni/s2pro-colocated-96gb.yaml](sglang-omni/s2pro-colocated-96gb.yaml)
is the GPU layout the measured results used: the reasoner, the recogniser, and
the speech model sharing one 96 GB card, with the memory split that leaves the
speech model enough headroom to stay ahead of playout under load. It is a
worked example rather than a requirement — the engine talks to model endpoints
over the network and does not care where they run.
