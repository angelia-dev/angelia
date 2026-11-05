// Package secrets stores secret values encrypted at rest (story #75,
// ADR 0024).
//
// A secret is an opaque value of at most [MaxValueBytes], stored once under
// a name in the secret table and referenced from configuration by a key
// ending in _ref. Values are sealed with AES-256-GCM under a data key
// derived with HKDF-SHA256 from a 32-byte master key read from a file
// (secrets.key_file) or from ANGELIA_SECRETS_KEY. The envelope carries a
// version byte, the key id and a random nonce, and its associated data binds
// it to the row's id and name, so a ciphertext copied into another row, or a
// row renamed in SQL, no longer opens.
//
// Plaintext only ever lives in a [Value], which prints, logs and encodes as
// [REDACTED]; [Value.Reveal] returns a copy of the bytes where a request is
// built. Errors never carry a value, a ciphertext or key material.
//
// A [Resolver] turns a reference into a value, refusing a secret bound to
// hosts (ADR 0024 §2) for any other destination. [CheckStartup] refuses to
// start without a key while secrets are stored, and [Rotate] re-seals every
// row under the primary key.
package secrets
