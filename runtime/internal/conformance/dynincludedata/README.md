# Dynamic Include conformance corpus (tv-dyninclude.yaml)

Source: `.squad/decisions/inbox/tess-dynamic-include-vectors.md` as of 2026-08-15.

This directory contains dynamic runbook include conformance vectors and the minimal JSON Schema that validates them.

## Status

The in-repository harness is the authority for adopted vectors. Individual
vectors may report an explicit unsupported reason; silent skips are forbidden.

## Maintenance

Merge by stable case ID. Identical duplicates may be deduplicated; differing
collisions must fail visibly rather than being synchronized from an external
checkout.

## Coverage

Group | Vectors
------|---------
Reference syntax / rejection | TV-DYN-REF-001..030
Template rendering | TV-DYN-TMPL-001..015
Catalog resolution | TV-DYN-CAT-001..015
Not-found behaviour | TV-DYN-NFD-001..010
Input validation + binding | TV-DYN-INPUT-001..011
Cycle + depth | TV-DYN-CYCLE-001..010
Governance composition | TV-DYN-GOV-001..020
Tools + package deps | TV-DYN-TOOL-001..005
Trace | TV-DYN-TRACE-001..007
Replay / resume drift | TV-DYN-REPLAY-001..008
Preview / dry-run | TV-DYN-PREV-001..008
Compatibility | TV-DYN-COMPAT-001..008
Schema structural | TV-DYN-SCHEMA-001..013
End-to-end scenario | TV-DYN-E2E-001..005
