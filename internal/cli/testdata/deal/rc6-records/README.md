# Records sealed by v0.1.0-rc6

`jet-ski-deal.json` is the `deal export` of the DEMO jet ski deal
(`skills/deal/demo/jet-ski`), sealed by `capsulectl` built from commit
`14bf3bc` (v0.1.0-rc6), byte for byte as exported. Its `check` and `verdict`
records carry `pack_id`, as every deal record of that release does.

It is kept so the profile's tests prove that records sealed before `pack_id`
stopped being written still validate against the current schema. Do not
regenerate it with a newer build.

Steps: the ones `skills/deal/scripts/run-demo.sh` runs at that commit, with
`CAPSULE_DEAL_CHECK_URL` unset, ending in `deal export --deal <id>`.
