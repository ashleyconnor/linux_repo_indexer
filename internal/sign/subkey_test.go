package sign

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// A conventional OpenPGP key certifies with its primary and signs with a
// subkey. Signing with the primary instead yields a signature apt rejects as
// NO_PUBKEY — naming a key that is sitting in the keyring — because the
// primary is not flagged for signing.
//
// These tests use the committed GnuPG-generated fixture, which has that
// structure. A key generated in-process with a sign-capable primary and no
// subkey would let the bug through.

func fixtureKey(t *testing.T) (private, public []byte) {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "signing")

	private, err := os.ReadFile(filepath.Join(dir, "test-key.private.asc"))
	if err != nil {
		t.Fatalf("reading fixture private key: %v", err)
	}
	public, err = os.ReadFile(filepath.Join(dir, "test-key.public.asc"))
	if err != nil {
		t.Fatalf("reading fixture public key: %v", err)
	}
	return private, public
}

func TestFixtureKeyHasASigningSubkey(t *testing.T) {
	// Guards the guard: if the fixture is ever regenerated without a signing
	// subkey, the tests below stop covering what they claim to.
	_, public := fixtureKey(t)

	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(public))
	if err != nil {
		t.Fatalf("reading fixture key: %v", err)
	}
	entity := keyring[0]

	if len(entity.Subkeys) == 0 {
		t.Fatal("the fixture key has no subkeys, so it cannot cover subkey signing")
	}
	signer, ok := entity.SigningKey(entity.PrimaryKey.CreationTime)
	if !ok {
		t.Fatal("the fixture key has no usable signing key")
	}
	if signer.PublicKey.KeyId == entity.PrimaryKey.KeyId {
		t.Fatal("the fixture key signs with its primary, so it cannot cover subkey signing")
	}
}

func TestClearSignUsesTheSigningSubkey(t *testing.T) {
	ctx := context.Background()
	private, public := fixtureKey(t)

	signer := NewPGPSigner(StaticKeySource{Armored: private})
	release := []byte("Origin: HashiCorp\nCodename: noble\n")

	inRelease, err := signer.ClearSign(ctx, release)
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}
	if _, err := VerifyClearSigned(public, inRelease); err != nil {
		t.Fatalf("InRelease does not verify against the published public key: %v", err)
	}
}

func TestDetachSignUsesTheSigningSubkey(t *testing.T) {
	ctx := context.Background()
	private, public := fixtureKey(t)

	signer := NewPGPSigner(StaticKeySource{Armored: private})
	repomd := []byte("<repomd><revision>1</revision></repomd>")

	sig, err := signer.DetachSign(ctx, repomd)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if err := Verify(public, repomd, sig); err != nil {
		t.Fatalf("detached signature does not verify against the published public key: %v", err)
	}
}

func TestBothSignatureFormsUseTheSameKey(t *testing.T) {
	// apt fetches InRelease and Release.gpg for the same document. If the two
	// were signed by different keys, one of them would fail verification for a
	// client that trusts only what we published.
	ctx := context.Background()
	private, public := fixtureKey(t)

	signer := NewPGPSigner(StaticKeySource{Armored: private})
	release := []byte("Origin: HashiCorp\nCodename: noble\n")

	inRelease, err := signer.ClearSign(ctx, release)
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}
	detached, err := signer.DetachSign(ctx, release)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}

	if _, err := VerifyClearSigned(public, inRelease); err != nil {
		t.Errorf("InRelease: %v", err)
	}
	if err := Verify(public, release, detached); err != nil {
		t.Errorf("Release.gpg: %v", err)
	}
}
