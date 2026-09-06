# Security policy

## Supported versions

v0.1.0 is the first release. Security fixes land on `main` and in the next
patch release; only the latest release and `main` are supported.

## Reporting a vulnerability

Report privately using GitHub's repository security advisory feature. If that
is unavailable, contact the repository owner through the address on the GitHub
profile. Do not include live credentials, private audio, or unnecessary
personal data in a report — a reproduction that needs real user audio can be
described rather than attached.

Include the affected version or commit, reproduction steps, impact, and a
proposed mitigation when you have one. Maintainers acknowledge within seven
days, coordinate a fix and a disclosure timeline, and credit the reporter
unless anonymity is requested.

## What this system treats as untrusted

Three boundaries carry most of the security weight, and a report against any of
them is a security report rather than a bug report:

**Observed content is evidence, never instruction.** What a screen, a document,
or a tool result says arrives with observer authority and is fenced. Text that
reaches the model through observation and is nonetheless obeyed as an
instruction is an authority failure. The injection-authority test is a release
gate; see [docs/safety.md](docs/safety.md).

**Computer use commits through one boundary.** Every `computer.*` action
declares its target and its confirmation requirement, and dispatch re-checks
authority against the trajectory rather than trusting the call site. An action
that reaches a surface without passing that boundary is a security defect
regardless of what it did.

**The wire is validated in both directions on every session.** A client event
that should have been rejected and was not — or a server event that does not
match the pinned schema — is a compatibility failure with security
consequences, because downstream code is entitled to assume the shape.

## What it does not defend against

The server trusts its own configuration: model endpoints, credentials, and
browser targets are supplied by the operator and are not sandboxed from each
other. A malicious model endpoint can say anything the binding will act on. If
you run computer use, the browser target is inside the trust boundary and
should be a dedicated profile rather than a person's own session — see
[docs/safety.md](docs/safety.md).
