# Contributor guidance

This repository will contain a CLI wrapper over emit, artifact storage, and CLL.

- Keep reusable storage in capsule-emit-go/artifact, not in this CLI repository.
- Reuse emit and CLL libraries; do not reimplement AAC digest/signature rules.
- Preserve exact bytes and explicit digest bindings; unbound attachments are not
  authenticated Capsule content. Trusted keys come from deployment policy.
- Use parameterized SQL, check iteration/close errors, and preserve transaction
  ownership. Never access production databases from tests.
- Use testify assert/require in tests. Keep application-specific one-off migration
  tools and fixtures outside this repository.
- Before an authorized commit, format, tidy modules, run tests and vet. Use DCO
  sign-off and conventional commit messages. Never commit secrets or local plans.
