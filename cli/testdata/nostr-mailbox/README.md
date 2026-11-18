# Nostr mailbox carriage vectors (EXPERIMENTAL)

Vectors for `spec/bindings/nostr-mailbox-v0.md`, an experimental carriage of the
`evidence-interop/1` exchange through a Nostr relay, and its adapter declaration
`spec/adapters/nostr-mailbox-v0.md`. The binding is not part of `evidence-interop/1` and has no
binding token. These vectors are not conformance vectors for that profile.

- `vectors.json`: the cases (binding §9). Each carries the request octets, the request event, the
  events a relay delivered, and, where the window closed with no reply, the absence record and any
  events delivered after it, with the expected classification of each event and the expected
  verifier results.
- `manifest.json`: the set, its status and the SHA-256 of each generated file.
- `SHA256SUMS`: every file here.

The envelopes, refusal, keys, log and contract pin are those of `vectors/interop-profile-v1/`.
Every Nostr key is a test key derived from a public label (`nostr_test_keys` in `vectors.json`).
The events are constructed. No relay was contacted, and `wss://relay.example` is an example name.

Regenerate with `python python/scripts/generate_nostr_mailbox_vectors.py`; check with
`pytest python/tests/test_nostr_mailbox_vectors.py`. The workflow
`.github/workflows/nostr-mailbox.yml` fails when a regeneration differs from these files.
