# Maintained examples

`v1/reference` consumes the stable `api/v1` interfaces directly through the
versioned key-free reference adapters. From the repository root:

```bash
go run ./examples/v1/reference
```

It streams the public PCM16 fixture, finalizes perception, creates a response
candidate, and consumes five output chunks. The example is compiled by the M7
reproduction gate and requires no Python, API key, network service, microphone,
or proprietary client.
