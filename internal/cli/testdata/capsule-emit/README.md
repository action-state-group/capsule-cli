# A bare capsule sealed by capsule-emit

`bare-capsule-0.8.6.json` is an Agent Action Capsule as the Python capsule-emit 0.8.6 seals it: the capsule's own fields plus
an inline `signature` (the hex COSE_Sign1 producer envelope over `capsule_id`) and `key_id` (the producer's raw Ed25519 public
key, hex).

It was generated in a scratch virtual environment, with witnessing off, so nothing left the machine:

```python
import json, capsule_emit
signer = capsule_emit.LocalKeypairSigner("./producer.key")
result = capsule_emit.seal({"check": "config file", "result": "same"}, operator="example-operator",
                           developer="example-developer", signer=signer, witness=False)
json.dump(result.capsule, open("bare-capsule-0.8.6.json", "w"), sort_keys=True, indent=1)
```

The private key was discarded. `capsulectl verify --capsule` checks the file against the key in its `key_id`.
