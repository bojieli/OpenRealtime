package presentation

// DefaultLoopbackHostAddress is the operator-overridable local address used by
// the standalone presentation host and bundled native endpoint directories.
// The clean Realtime server remains a separate process on its own public API;
// this address belongs only to the optional presentation layer.
const DefaultLoopbackHostAddress = "127.0.0.1:8767"
