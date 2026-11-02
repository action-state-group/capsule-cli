### `verify --history` reads personal_history_outcome/1 and inference-service-history/0 (capsule-cli#523)

- **Added:** `verify --history` evaluates a `personal_history_outcome/1` file `{history, definition, exchanges, evidence, openings}` and an `inference-service-history/0` file `{history, definition, request, reply}`, with agent-action-capsule's readers (go/v0.20.0). The keys and policy are yours, given by flag, never read from the file: `--holder-key` for responders, `--log-key`, `--witness-key` and `--peer-key` for the service History's pins, and `--outcome-policy` (predicate table, role keys, requirements) for the Outcome History. Each property is `PASS`, `FAIL`, `INCONCLUSIVE`, `NOT_CHECKED` or `NOT_PRESENT`, with no score and no sum across instances. A native object's check is `NOT_CHECKED`: this verifier loads no adapter.
- **Changed:** the two instances were `not_evaluable` until now.
- **Changed (dependency):** agent-action-capsule/go v0.19.0 to v0.20.0.
