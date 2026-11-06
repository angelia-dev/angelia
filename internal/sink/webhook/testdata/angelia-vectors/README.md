# Angelia webhook vectors

`vectors.json` is copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `d9019f6` ("feat(webhooks): publish Standard Webhooks test vectors for Angelia deliveries (#11)"),
path `webhooks/v1/vectors.json`.

- **sha256:** `1f13737bcf41e121cb8536f4e26749aa0f2d41a689c33e8d0c7be2502ab6e712`

The spec repository is the source of truth: never edit this file here. To update, copy the file
from a newer spec commit, record the commit and sha256 above, and run `go test ./internal/sink/webhook/`.
`angelia_vectors_test.go` runs every case through `Sign` and `Verify`.

Licence: CC-BY-4.0, as stated in the file's own `description`.
