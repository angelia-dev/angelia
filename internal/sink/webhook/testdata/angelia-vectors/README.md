# Angelia webhook vectors

`vectors.json` is copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `4e93bc6` ("fix(webhooks): vector bodies carry urls.repository, signatures recomputed", branch `story/84-urls-repository`; first published at `d9019f6`, #11),
path `webhooks/v1/vectors.json`.

- **sha256:** `d72fb8781f9a0f391d999033da11394d150cbdbbe03175949015c27345895487`

The spec repository is the source of truth: never edit this file here. To update, copy the file
from a newer spec commit, record the commit and sha256 above, and run `go test ./internal/sink/webhook/`.
`angelia_vectors_test.go` runs every case through `Sign` and `Verify`.

Licence: CC-BY-4.0, as stated in the file's own `description`.
