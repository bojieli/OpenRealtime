// Package sdkclient holds the test that runs OpenAI's own Realtime client
// against an OpenRealtime server.
//
// There is no Go code here to speak of. The client is OpenAI's, in JavaScript,
// installed from npm and unmodified; this package exists so that `go test`
// runs it, because a compatibility claim that lives outside the gate is a
// compatibility claim nobody checks.
package sdkclient
