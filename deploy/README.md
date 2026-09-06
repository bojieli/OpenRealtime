# Deployment

## Container

```bash
export GEMINI_API_KEY="your-key"
export OPENREALTIME_TOKEN="replace-with-a-long-random-token"

docker run --rm -p 8080:8080 \
  -e GEMINI_API_KEY \
  -e OPENREALTIME_TOKEN \
  ghcr.io/bojieli/openrealtime:0.1.0 serve \
    -listen :8080 \
    -binding upstream \
    -upstream-provider google \
    -slow-provider google
```

The release workflow is configured to publish images for `linux/amd64` and
`linux/arm64` on version tags. To build an image locally:

```bash
docker build -f deploy/Dockerfile -t openrealtime:0.1.0 .
```

This example uses Gemini Live for the foreground voice and Gemini for background
reasoning, and requires the exported bearer token from Realtime clients.
Replace the provider flags as you would for a source build. A local model
endpoint bound to the host is not container loopback; give the container an
explicit reachable address.

`-listen :8080` is a routable address, so the server refuses to start without a
bearer token. That refusal is the point: a container with a published port and
no token is an open Realtime endpoint spending your model credentials. Bind
loopback instead if you genuinely want no authentication.

The image runs as a non-root user and contains the server binary and
certificate roots. It has no shell, package manager, or model weights.

Supply configuration at launch through environment variables, flags, or a
mounted YAML file; never bake credentials into the image. The same image can
therefore run in every environment. `openrealtime serve -h` lists the flags,
and [../docs/operations.md](../docs/operations.md) covers what to set and why.

## Binaries

The release workflow produces static binaries for linux/amd64, linux/arm64,
darwin/arm64, darwin/amd64, and windows/amd64 with a `SHA256SUMS` file. Check the
[releases page](https://github.com/bojieli/OpenRealtime/releases) for published
artifacts. To build them yourself:

```bash
./scripts/build-release.sh dist
```

The builds are reproducible: the same source tree and Go toolchain produce
byte-identical binaries anywhere, which is what makes the checksums worth
publishing. CI verifies that property on every change, and again on the exact
artifacts a release publishes, rather than taking it on faith.

Every binary reports what it was built from:

```console
$ openrealtime version
openrealtime 0.1.0, revision 4f2c1a…, go go1.25.0
```

A tree with uncommitted changes is reported as `(modified)`, so a binary found
running somewhere can always be traced back to a source tree. The release
workflow refuses to publish when the tag, the `VERSION` file, and what the
binary says about itself do not name the same release.

## Exposing it: Cloudflare Tunnel

The server serves plain HTTP and WebSocket. Terminate TLS in a reverse proxy
or tunnel so it handles certificates and renewal independently of live agent
sessions.

The shape this project deploys with is a [Cloudflare
Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/):
`cloudflared` dials out to Cloudflare and forwards to a loopback origin. TLS is
terminated at the edge, the origin has no inbound port at all, and there is no
certificate on the box.

```bash
cloudflared tunnel login
cloudflared tunnel create openrealtime
cloudflared tunnel route dns openrealtime realtime.example.com
```

Point it at the loopback listener — [cloudflared/config.example.yml](cloudflared/config.example.yml):

```yaml
tunnel: openrealtime
credentials-file: /etc/cloudflared/openrealtime.json

ingress:
  - hostname: realtime.example.com
    service: http://127.0.0.1:8765
  - service: http_status:404
```

Then run the two halves:

```bash
export OPENREALTIME_TOKEN="replace-with-a-long-random-token"
openrealtime serve -listen 127.0.0.1:8765 &
cloudflared tunnel run openrealtime
```

Clients connect to `wss://realtime.example.com/v1/realtime` with
`Authorization: Bearer $OPENREALTIME_TOKEN`. WebSockets are proxied without
extra configuration, and the server's default 20-second keepalive ping keeps an
idle session alive through the edge rather than letting an inactive connection
be reaped mid-conversation.

Set `OPENREALTIME_TOKEN` even when the origin binds to loopback. A tunnel makes
that origin publicly reachable, which the server cannot determine from its
listen address. The non-loopback startup check does not cover this case.

For a second, revocable layer, put [Cloudflare
Access](https://developers.cloudflare.com/cloudflare-one/policies/access/) in
front of the hostname. A bearer token is one secret shared by every client;
Access is per-identity and can be withdrawn without redeploying anything.

Two things do not travel through an HTTP tunnel:

- **WebRTC media.** The SDP offer is an ordinary POST and goes through, but the
  media path is UDP that the tunnel does not carry. Over a tunnel, use the
  WebSocket transport; for WebRTC at scale use the LiveKit path in
  [../docs/transports.md](../docs/transports.md), which is what it exists for.
- **The browser companion.** `openrealtime companion` is a loopback developer
  client and refuses a non-loopback listen address. What you publish is the
  Realtime endpoint; the client is an official OpenAI Realtime SDK, or your own.

`serve` exposes exactly three routes — `/v1/realtime`, `/healthz`, and
`/metrics`. With a token configured, `/metrics` and the detailed half of
`/healthz` require it; the health status word stays public so an external check
can read it.

Any other terminating proxy works the same way: give it the loopback origin and
let it own TLS. There is nothing tunnel-specific in the server.

## Colocated model serving

[sglang-omni/s2pro-colocated-96gb.yaml](sglang-omni/s2pro-colocated-96gb.yaml)
is the GPU layout the measured results used: the reasoner, the recogniser, and
the speech model sharing one 96 GB card, with the memory split that leaves the
speech model enough headroom to stay ahead of playout under load. It is a
worked example rather than a requirement — the engine talks to model endpoints
over the network and does not care where they run.
