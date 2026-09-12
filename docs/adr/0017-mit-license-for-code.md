# ADR-0017: License the code under MIT

- Status: accepted
- Date: 2026-09-12
- Supersedes in part: [ADR-0002](0002-licensing-and-artifact-provenance.md)

## Context

[ADR-0002](0002-licensing-and-artifact-provenance.md) set the repository's
licensing shape: Apache-2.0 for code, CC BY 4.0 for documentation, CC0-1.0 for
original fixtures, and a provenance record for every nontrivial artifact. The
separation and the provenance requirement have held up. The choice of code
license is the only part revisited here.

Apache-2.0 and MIT both permit commercial use, modification, and
redistribution. They differ in what they ask of a user and what they give:
Apache-2.0 carries an express patent grant with a termination clause, requires
that modified files be marked, and requires that a NOTICE file be carried
forward; MIT asks only that the copyright notice and permission notice travel
with the software.

## Decision

Code, schemas, configuration, and scripts are licensed under the MIT License.
Documentation remains CC BY 4.0. Golden traces, recorded images, and contract
fixtures remain CC0-1.0. The speech fixtures remain outside any grant this
project can make, for the reason recorded in
[LICENSES.md](../../LICENSES.md): they are the output of a model whose own
license governs them.

The provenance obligation from ADR-0002 is unchanged and is not weakened by
this decision. Every nontrivial artifact still records origin, applicable
license, lawful basis, transformations, and a cryptographic hash.

## Consequences

Adopters get the shorter and more permissive of the two licenses, and no longer
inherit Apache-2.0's file-marking or NOTICE-carrying obligations.

They also no longer receive Apache-2.0's express patent grant. MIT conveys
rights in copyright and is generally read to imply a patent license only so far
as the granted rights require; it states nothing explicitly, and it has no
patent-retaliation clause. An adopter for whom an express grant matters should
weigh that, and this file is the record that the trade was made deliberately
rather than by omission.

Relicensing was available because every commit in this repository is the work
of a single copyright holder. That will stop being true as soon as the project
takes outside contributions, so the decision is recorded now, while it is still
cheap to state.
