module github.com/bojieli/OpenRealtime

go 1.25.0

// The patched 1.25 line, not merely "some 1.25".
//
// govulncheck reports the standard library that a build actually used, and
// 1.25.0 carries twenty-six advisories this code reaches - in crypto/tls,
// crypto/x509, net/http, net/url, encoding/asn1, and os - every one of them
// fixed in a 1.25 patch release. A module that asks only for 1.25 gets
// whichever patch the machine happens to have, which on a long-lived build
// host is the one it was installed with.
toolchain go1.25.14

require (
	github.com/coder/websocket v1.8.15
	github.com/pion/interceptor v0.1.47
	github.com/pion/webrtc/v4 v4.2.19
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.41.0
	gopkg.in/hraban/opus.v2 v2.0.0-20230925203106-0188a62cb302
)

require golang.org/x/image v0.45.0

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.5 // indirect
	github.com/pion/ice/v4 v4.4.0 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/opus v0.1.0
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.18
	github.com/pion/rtp v1.10.5
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/pion/stun/v3 v3.1.7 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	github.com/pion/turn/v5 v5.0.13 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	gopkg.in/yaml.v3 v3.0.1
)
