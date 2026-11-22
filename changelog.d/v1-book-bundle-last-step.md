### Fixed

- `verify --bundle` accepts the standalone report of a real x-deal-v1 deal-book bundle. A deal step in a book's bundle has no embedded `evidence_record_header`; its header is the record's disclosed `agent_input`, and the report check now reads it from there, only when it matches the digest the record's Capsule commits. Before this, every such bundle failed with `sealed_report_unverified` (capsule-cli#604).
