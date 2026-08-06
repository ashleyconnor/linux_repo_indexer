// Package sign produces the OpenPGP signatures apt and dnf verify.
//
// AWS KMS cannot do OpenPGP, so the private key lives in Secrets Manager and
// signing happens in the publisher. The key must be the one clients already
// trust: signing with a fresh key breaks apt-get update for everyone.
package sign

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	_ "crypto/sha256" // registers SHA-256 for signing
)

// signConfig pins how signatures are produced.
//
// go-crypto adds a salt notation to signatures by default, a draft
// crypto-refresh feature. GnuPG ignores the unknown notation and reports a
// good signature, but librepo — which is what dnf actually verifies
// repomd.xml.asc with — rejects the signature outright. Turning it off also
// makes signatures deterministic.
func signConfig() *packet.Config {
	noSalt := false
	return &packet.Config{
		DefaultHash:                           crypto.SHA256,
		NonDeterministicSignaturesViaNotation: &noSalt,
	}
}

// A Signer produces the two signature forms a repository needs.
type Signer interface {
	// ClearSign wraps the document and its signature together, which is what
	// apt reads as InRelease.
	ClearSign(ctx context.Context, doc []byte) ([]byte, error)

	// DetachSign returns an armored signature over the document, published
	// alongside it as Release.gpg or repomd.xml.asc.
	DetachSign(ctx context.Context, doc []byte) ([]byte, error)

	// PublicKey returns the armored public key, so it can be published for
	// clients to import.
	PublicKey(ctx context.Context) ([]byte, error)
}

// A KeySource supplies the armored private key and its passphrase. Secrets
// Manager is the production implementation; tests supply an ephemeral key.
type KeySource interface {
	PrivateKey(ctx context.Context) (armoredKey []byte, passphrase string, err error)
}

// PGPSigner signs with an OpenPGP key fetched from a KeySource.
//
// The key is fetched once and cached for the lifetime of the process, which on
// Lambda means once per container rather than once per publish.
type PGPSigner struct {
	source KeySource

	// Now overrides the clock used when selecting a valid signing key.
	Now func() time.Time

	once   sync.Once
	entity *openpgp.Entity
	err    error
}

// NewPGPSigner returns a signer that reads its key from source.
func NewPGPSigner(source KeySource) *PGPSigner {
	return &PGPSigner{source: source}
}

// now is the clock used to select a signing key that is currently valid.
// Injectable so tests can reason about expiry.
func (s *PGPSigner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *PGPSigner) load(ctx context.Context) (*openpgp.Entity, error) {
	s.once.Do(func() {
		armored, passphrase, err := s.source.PrivateKey(ctx)
		if err != nil {
			s.err = fmt.Errorf("sign: fetching private key: %w", err)
			return
		}

		keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
		if err != nil {
			s.err = fmt.Errorf("sign: reading private key: %w", err)
			return
		}
		if len(keyring) != 1 {
			s.err = fmt.Errorf("sign: expected exactly one key, got %d", len(keyring))
			return
		}

		entity := keyring[0]
		if entity.PrivateKey == nil {
			s.err = fmt.Errorf("sign: the configured secret holds a public key, not a private one")
			return
		}
		if entity.PrivateKey.Encrypted {
			if passphrase == "" {
				s.err = fmt.Errorf("sign: private key is passphrase-protected but no passphrase was supplied")
				return
			}
			if err := entity.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
				s.err = fmt.Errorf("sign: decrypting private key: %w", err)
				return
			}
			for _, sub := range entity.Subkeys {
				if sub.PrivateKey != nil && sub.PrivateKey.Encrypted {
					// A failure here is not fatal: the primary key may be the
					// signing key.
					_ = sub.PrivateKey.Decrypt([]byte(passphrase))
				}
			}
		}
		s.entity = entity
	})
	return s.entity, s.err
}

// ClearSign returns the document wrapped in a clearsigned message.
func (s *PGPSigner) ClearSign(ctx context.Context, doc []byte) ([]byte, error) {
	entity, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	// The entity's designated signing key, which for a conventionally
	// structured key is a subkey rather than the primary. Signing with the
	// primary instead produces a signature apt rejects as NO_PUBKEY even
	// though the key it names is right there in the keyring, because the
	// primary is not flagged for signing.
	signer, ok := entity.SigningKey(s.now())
	if !ok {
		return nil, fmt.Errorf("sign: key %X has no usable signing key", entity.PrimaryKey.KeyId)
	}

	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, signer.PrivateKey, signConfig())
	if err != nil {
		return nil, fmt.Errorf("sign: starting clearsign: %w", err)
	}
	if _, err := w.Write(doc); err != nil {
		return nil, fmt.Errorf("sign: clearsigning: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("sign: finishing clearsign: %w", err)
	}
	// clearsign omits the trailing newline apt expects after the signature.
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// DetachSign returns an armored detached signature.
func (s *PGPSigner) DetachSign(ctx context.Context, doc []byte) ([]byte, error) {
	entity, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, entity, bytes.NewReader(doc), signConfig()); err != nil {
		return nil, fmt.Errorf("sign: detached signing: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// PublicKey returns the armored public half of the signing key.
func (s *PGPSigner) PublicKey(ctx context.Context) ([]byte, error) {
	entity, err := s.load(ctx)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, fmt.Errorf("sign: armoring public key: %w", err)
	}
	if err := entity.Serialize(w); err != nil {
		return nil, fmt.Errorf("sign: serialising public key: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("sign: armoring public key: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// StaticKeySource holds a key in memory. It backs the test signer and the
// seeder's local verification, never production.
type StaticKeySource struct {
	Armored    []byte
	Passphrase string
}

// PrivateKey implements KeySource.
func (s StaticKeySource) PrivateKey(context.Context) ([]byte, string, error) {
	return s.Armored, s.Passphrase, nil
}

// Verify checks a detached signature against a document using an armored
// public key. The publisher does not need it, but the seeder's verification
// mode and the end-to-end tests do.
func Verify(publicKey, doc, signature []byte) error {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(publicKey))
	if err != nil {
		return fmt.Errorf("sign: reading public key: %w", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(doc), bytes.NewReader(signature), nil); err != nil {
		return fmt.Errorf("sign: verifying signature: %w", err)
	}
	return nil
}

// VerifyClearSigned checks a clearsigned document and returns its payload.
func VerifyClearSigned(publicKey, clearsigned []byte) ([]byte, error) {
	block, _ := clearsign.Decode(clearsigned)
	if block == nil {
		return nil, fmt.Errorf("sign: not a clearsigned document")
	}

	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(publicKey))
	if err != nil {
		return nil, fmt.Errorf("sign: reading public key: %w", err)
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil); err != nil {
		return nil, fmt.Errorf("sign: verifying clearsigned document: %w", err)
	}
	return block.Plaintext, nil
}

// GenerateTestKey creates a throwaway signing key.
//
// It exists so the end-to-end tests can sign a repository and import the
// matching public key into a container, exercising the real verification path
// without a production key ever leaving Secrets Manager.
func GenerateTestKey(name, email string) (source StaticKeySource, publicKey []byte, err error) {
	entity, err := openpgp.NewEntity(name, "linux-repo-indexer test key", email, signConfig())
	if err != nil {
		return StaticKeySource{}, nil, fmt.Errorf("sign: generating test key: %w", err)
	}

	// SerializePrivate computes the identity and subkey self-signatures as a
	// side effect, and must run first. Without them GnuPG treats the public
	// half as having no valid binding and rejects every signature made by the
	// key — Go's own verifier is more forgiving, so this shows up only when a
	// real dnf or apt is asked to check the repository.
	private, err := armorEntity(openpgp.PrivateKeyType, func(w io.Writer) error {
		return entity.SerializePrivate(w, signConfig())
	})
	if err != nil {
		return StaticKeySource{}, nil, err
	}
	public, err := armorEntity(openpgp.PublicKeyType, entity.Serialize)
	if err != nil {
		return StaticKeySource{}, nil, err
	}
	return StaticKeySource{Armored: private}, public, nil
}

func armorEntity(blockType string, write func(io.Writer) error) ([]byte, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, blockType, nil)
	if err != nil {
		return nil, fmt.Errorf("sign: armoring %s: %w", blockType, err)
	}
	if err := write(w); err != nil {
		return nil, fmt.Errorf("sign: serialising %s: %w", blockType, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("sign: armoring %s: %w", blockType, err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
