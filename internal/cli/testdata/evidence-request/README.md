# evidence-request/1 answer vectors

Answers produced by the Python responder, `capsule_emit.evidence_request.answer`
(capsule-emit 0.8.2 from PyPI), over a freshly sealed three-record ledger with
one stub-witness checkpoint. The Go `book` verbs are tested against these bytes,
so a disagreement between the two implementations fails the Go tests.

| file | subject |
|---|---|
| `record.*` | one record by `capsule_id` |
| `range.*` | a two-record page of a three-record range (`next_page_token` is set) |
| `chain_segment.*` | the last checkpoint (`last: 1`), with its signed COSE statement |
| `no_such_record.*` | a record the ledger does not hold: a signed refusal |

Each `*.request.json` is the exact request the responder digested; the refusal's
`request_digest` is the SHA-256 of that file's bytes.

The stub witness never contacts a Transparency Service, so these checkpoints are
self-attested only. They test encoding and signatures, not witnessing.

Regenerate (the values change on every run; the tests do not pin them):

```
python3 -m venv venv && ./venv/bin/pip install 'capsule-emit==0.8.2'
CAPSULE_WITNESS=stub ./venv/bin/python generate.py .
```
