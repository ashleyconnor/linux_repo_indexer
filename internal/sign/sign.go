// Package sign produces the OpenPGP signatures apt and dnf verify.
//
// AWS KMS cannot do OpenPGP, so the private key lives in Secrets Manager and
// signing happens in the publisher. The key must be the one clients already
// trust: signing with a fresh key breaks apt-get update for everyone.
package sign

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

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

	once   sync.Once
	entity *openpgp.Entity
	err    error
}

// NewPGPSigner returns a signer that reads its key from source.
func NewPGPSigner(source KeySource) *PGPSigner {
	return &PGPSigner{source: source}
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

	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, entity.PrivateKey, nil)
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
	if err := openpgp.ArmoredDetachSign(&buf, entity, bytes.NewReader(doc), nil); err != nil {
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
	entity, err := openpgp.NewEntity(name, "linux-repo-indexer test key", email, nil)
	if err != nil {
		return StaticKeySource{}, nil, fmt.Errorf("sign: generating test key: %w", err)
	}

	private, err := armorEntity(entity, openpgp.PrivateKeyType, func(w io.Writer) error {
		return entity.SerializePrivateWithoutSigning(w, nil)
	})
	if err != nil {
		return StaticKeySource{}, nil, err
	}
	public, err := armorEntity(entity, openpgp.PublicKeyType, entity.Serialize)
	if err != nil {
		return StaticKeySource{}, nil, err
	}
	return StaticKeySource{Armored: private}, public, nil
}

func armorEntity(_ *openpgp.Entity, blockType string, write func(io.Writer) error) ([]byte, error) {
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
