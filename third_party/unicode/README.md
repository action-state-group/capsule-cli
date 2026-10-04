# Unicode data

`confusables.txt` is Unicode's TR39 confusables data (UTS #39, Unicode
Security Mechanisms), vendored unmodified from
`https://www.unicode.org/Public/<version>/security/confusables.txt`. Its terms of
use are at https://www.unicode.org/terms_of_use.html. `confusables.txt.sha256`
is its digest.

Refresh it with `scripts/update-confusables.sh VERSION`. That also regenerates
`internal/cli/confusables_table.go`, which holds the mappings alone.
