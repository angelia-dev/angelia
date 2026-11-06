# Standard Webhooks test vectors

Vendored for `internal/sink/webhook` (ADR 0023 section 7, acceptance criterion 1 of #108).

- **Source:** <https://github.com/standard-webhooks/standard-webhooks>
- **Pinned commit:** `ab6844147ce9cdb0d2dbf2e60d1ea85001fd2554` (`main`, fetched 2026-11-05)
- **File:** `libraries/go/webhook_test.go`, copied unchanged as `webhook_test.go.txt`
  (renamed so that Go does not build it)
  - sha256 `545ddbf7023d9c2ae90ca0778a4a6ac988aa815dadc5f0273dc8af228b3b0141`

## Licence

- The repository's root `LICENSE` is Apache-2.0: copied as `LICENSE`.
- The `libraries/` directory, which holds the vectors' file, has its own MIT licence,
  "Copyright (c) 2023 Svix": copied as `LICENSE-libraries`.

The pinned commit has no `NOTICE` file. Both licences are on the list AGENTS.md allows.

## `vectors.json`

`vectors.json` holds the cases of `webhook_test.go.txt` as data, transcribed by hand:

- `sign`: `TestWebhookSign` (secret, id, timestamp, body and the expected `webhook-signature`).
- `verify`: every case of `TestWebhook`, plus `TestWebhookPrefix`, `TestWebhookNoEmptySecrets` and
  `TestWebhookNoEmptyPrefixedSecrets`. The reference cases sign with `time.Now()`; here every case uses the
  fixed timestamp and signature of `TestWebhookSign`, and `now` is the receiver's clock in Unix seconds.
  `error` names the sentinel `Verify` must return (`missing_headers`, `timestamp`,
  `no_matching_signature`, `invalid_secret`).

Adapted cases, marked in their name:

- **Tolerance bounds.** The reference's "old timestamp fails" signs at exactly `now - 5 min` and fails
  only because time passes before it verifies. ADR 0023 section 7 makes the tolerance inclusive at both
  ends, so the vectors check both bounds: `±300 s` passes, `±301 s` fails.
- **"invalid timestamp fails"** uses `VerifyIgnoringTimestamp` in the reference. `Verify` has no such
  mode, so `now` is set to the forged timestamp: the tolerance passes and the signature must not match.

The challenge-response tests (`TestChallengeResponse`, `TestVerifyChallengeResponse`) cover an optional
endpoint-verification feature that Angelia does not implement, and are not transcribed.
