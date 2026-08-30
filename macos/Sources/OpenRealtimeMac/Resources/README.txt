Runtime resources copied by SwiftPM live in this directory. The browser-use
bridge is installed beside them by build-app.sh so it remains independently
updatable and auditable.

Each native client manifest has a sibling versioned endpoint directory. The
directory contains only exact credential-free public-wire URLs and protocols;
session tokens and effect authority are never resource data.
