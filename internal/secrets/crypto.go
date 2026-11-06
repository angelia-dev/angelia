package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Envelope layout (ADR 0024 §3): byte 0 the version, bytes 1-8 the key id,
// bytes 9-20 the nonce, then the GCM ciphertext and its 16-byte tag.
const (
	envelopeVersion = 0x01
	keyIDLen        = 8
	nonceLen        = 12
	tagLen          = 16
	headerLen       = 1 + keyIDLen
	minEnvelope     = headerLen + nonceLen + tagLen
	// KeySize is the size of a master key in bytes.
	KeySize = 32
	// MaxValueBytes is the largest secret value (64 KiB).
	MaxValueBytes = 64 << 10
)

// HKDF info strings: the data key and the key id derive from the master
// independently, so a later use of the master gets a key of its own.
const (
	infoDataKey = "angelia secrets v1 aes-256-gcm"
	infoKeyID   = "angelia secrets v1 key id"
	aadPrefix   = "angelia/secret/v1"
)

var (
	// ErrDecrypt is wrapped by every failure to open an envelope: a wrong
	// key, a tampered or truncated envelope, or one moved to another row.
	// Its message names the secret and the key id only.
	ErrDecrypt = errors.New("secrets: cannot decrypt")
	// ErrUnknownKey is wrapped when an envelope names a key id that is not
	// configured, or has a version this build does not know.
	ErrUnknownKey = errors.New("secrets: unknown key")
)

// key is one master key, reduced to its id and its AEAD; the master itself
// is not kept.
type key struct {
	id   string
	aead cipher.AEAD
}

// Keyring holds the primary key, which seals, and the previous keys, which
// only open (during a rotation, or to read a restored backup).
type Keyring struct {
	primary *key
	byID    map[string]*key
	ids     []string
}

// NewKeyring builds a keyring from 32-byte master keys: primary seals, and
// every key opens. A previous key equal to the primary is ignored.
func NewKeyring(primary []byte, previous ...[]byte) (*Keyring, error) {
	p, err := newKey(primary)
	if err != nil {
		return nil, fmt.Errorf("secrets: primary key: %w", err)
	}
	k := &Keyring{primary: p, byID: map[string]*key{p.id: p}, ids: []string{p.id}}
	for i, m := range previous {
		pk, err := newKey(m)
		if err != nil {
			return nil, fmt.Errorf("secrets: previous key %d: %w", i+1, err)
		}
		if _, dup := k.byID[pk.id]; dup {
			continue
		}
		k.byID[pk.id] = pk
		k.ids = append(k.ids, pk.id)
	}
	return k, nil
}

func newKey(master []byte) (*key, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("want %d bytes, got %d", KeySize, len(master))
	}
	dk, err := hkdf.Key(sha256.New, master, nil, infoDataKey, 32)
	if err != nil {
		return nil, fmt.Errorf("derive data key: %w", err)
	}
	idb, err := hkdf.Key(sha256.New, master, nil, infoKeyID, keyIDLen)
	if err != nil {
		return nil, fmt.Errorf("derive key id: %w", err)
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &key{id: hex.EncodeToString(idb), aead: aead}, nil
}

// PrimaryID is the hex id of the key that seals.
func (k *Keyring) PrimaryID() string { return k.primary.id }

// IDs lists the configured key ids, the primary first.
func (k *Keyring) IDs() []string { return append([]string(nil), k.ids...) }

// Has reports whether id is a configured key id.
func (k *Keyring) Has(id string) bool {
	_, ok := k.byID[id]
	return ok
}

// aad is the associated data of an envelope: it binds the ciphertext to its
// header and to the row's id and name. Neither a ULID nor a valid name holds
// NUL, so the encoding is unambiguous.
func aad(header []byte, id, name string) []byte {
	b := make([]byte, 0, len(aadPrefix)+1+len(header)+1+len(id)+1+len(name))
	b = append(b, aadPrefix...)
	b = append(b, 0)
	b = append(b, header...)
	b = append(b, 0)
	b = append(b, id...)
	b = append(b, 0)
	return append(b, name...)
}

// Seal encrypts plaintext for the row (id, name) under the primary key and
// returns the envelope and the key id. Each call draws a fresh random nonce,
// so two seals of one plaintext differ.
func (k *Keyring) Seal(id, name string, plaintext []byte) (envelope []byte, keyID string, err error) {
	if len(plaintext) > MaxValueBytes {
		return nil, "", fmt.Errorf("secrets: secret %s: value of %d bytes is larger than %d", name, len(plaintext), MaxValueBytes)
	}
	p := k.primary
	env := make([]byte, headerLen+nonceLen, headerLen+nonceLen+len(plaintext)+tagLen)
	env[0] = envelopeVersion
	if _, err := hex.Decode(env[1:headerLen], []byte(p.id)); err != nil {
		return nil, "", fmt.Errorf("secrets: key id: %w", err)
	}
	if _, err := rand.Read(env[headerLen:]); err != nil {
		return nil, "", fmt.Errorf("secrets: nonce: %w", err)
	}
	env = p.aead.Seal(env, env[headerLen:headerLen+nonceLen], plaintext, aad(env[:headerLen], id, name))
	return env, p.id, nil
}

// EnvelopeKeyID returns the hex key id an envelope names, or "" when it is
// too short to name one.
func EnvelopeKeyID(envelope []byte) string {
	if len(envelope) < headerLen {
		return ""
	}
	return hex.EncodeToString(envelope[1:headerLen])
}

// Open decrypts the envelope of the row (id, name). Every failure is an
// error naming the secret and the key id, never the bytes: ErrUnknownKey for
// a key id that is not configured or an unknown version, ErrDecrypt
// otherwise.
func (k *Keyring) Open(id, name string, envelope []byte) (Value, error) {
	kid := EnvelopeKeyID(envelope)
	if len(envelope) < minEnvelope {
		return Value{}, fmt.Errorf("%w secret %s (key id %s): envelope too short", ErrDecrypt, name, orUnknown(kid))
	}
	if envelope[0] != envelopeVersion {
		return Value{}, fmt.Errorf("%w: secret %s has envelope version %d (key id %s), which this build cannot read", ErrUnknownKey, name, envelope[0], kid)
	}
	kk, ok := k.byID[kid]
	if !ok {
		return Value{}, fmt.Errorf("%w: secret %s is sealed with key id %s, which is not configured; add its key file to secrets.previous_key_files", ErrUnknownKey, name, kid)
	}
	nonce := envelope[headerLen : headerLen+nonceLen]
	plain, err := kk.aead.Open(nil, nonce, envelope[headerLen+nonceLen:], aad(envelope[:headerLen], id, name))
	if err != nil {
		return Value{}, fmt.Errorf("%w secret %s (key id %s)", ErrDecrypt, name, kid)
	}
	return Value{b: plain}, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
