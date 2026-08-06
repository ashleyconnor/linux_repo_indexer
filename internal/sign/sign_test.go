package sign

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func testSigner(t *testing.T) (*PGPSigner, []byte) {
	t.Helper()
	source, public, err := GenerateTestKey("Fixture Signing Key", "fixtures@example.com")
	if err != nil {
		t.Fatalf("GenerateTestKey: %v", err)
	}
	return NewPGPSigner(source), public
}

func TestClearSignRoundTrip(t *testing.T) {
	ctx := context.Background()
	signer, public := testSigner(t)

	// A realistic Release file: apt clearsigns this into InRelease.
	doc := []byte("Origin: HashiCorp\nCodename: noble\nSHA256:\n abc 12 main/binary-amd64/Packages\n")

	signed, err := signer.ClearSign(ctx, doc)
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}

	if !bytes.HasPrefix(signed, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		t.Errorf("output is not a clearsigned message:\n%s", signed)
	}
	if !bytes.HasSuffix(signed, []byte("-----END PGP SIGNATURE-----\n")) {
		t.Errorf("output does not end with a complete signature block:\n%q", signed[len(signed)-40:])
	}

	payload, err := VerifyClearSigned(public, signed)
	if err != nil {
		t.Fatalf("VerifyClearSigned: %v", err)
	}
	// clearsign normalises line endings, so compare the trimmed payload.
	if got, want := strings.TrimSpace(string(payload)), strings.TrimSpace(string(doc)); got != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
}

func TestDetachSignRoundTrip(t *testing.T) {
	ctx := context.Background()
	signer, public := testSigner(t)

	doc := []byte("<repomd><revision>1785988475</revision></repomd>")

	sig, err := signer.DetachSign(ctx, doc)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if !bytes.HasPrefix(sig, []byte("-----BEGIN PGP SIGNATURE-----")) {
		t.Errorf("output is not an armored signature:\n%s", sig)
	}

	if err := Verify(public, doc, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsTamperedDocument(t *testing.T) {
	ctx := context.Background()
	signer, public := testSigner(t)

	doc := []byte("<repomd><revision>1</revision></repomd>")
	sig, err := signer.DetachSign(ctx, doc)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}

	tampered := []byte("<repomd><revision>2</revision></repomd>")
	if err := Verify(public, tampered, sig); err == nil {
		t.Fatal("a modified document must not verify; this is the whole point of signing repomd.xml")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	ctx := context.Background()
	signer, _ := testSigner(t)
	_, otherPublic := testSigner(t)

	doc := []byte("Origin: HashiCorp\n")
	sig, err := signer.DetachSign(ctx, doc)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}

	// Signing with a key clients do not trust is the failure mode that breaks
	// apt-get update for everyone, so it must be detectable.
	if err := Verify(otherPublic, doc, sig); err == nil {
		t.Fatal("a signature from an untrusted key must not verify")
	}
}

func TestVerifyClearSignedRejectsTampering(t *testing.T) {
	ctx := context.Background()
	signer, public := testSigner(t)

	signed, err := signer.ClearSign(ctx, []byte("Codename: noble\n"))
	if err != nil {
		t.Fatalf("ClearSign: %v", err)
	}

	tampered := bytes.Replace(signed, []byte("noble"), []byte("jammy"), 1)
	if _, err := VerifyClearSigned(public, tampered); err == nil {
		t.Fatal("a modified InRelease must not verify")
	}
}

func TestVerifyClearSignedRejectsUnsignedInput(t *testing.T) {
	_, public := testSigner(t)
	if _, err := VerifyClearSigned(public, []byte("Codename: noble\n")); err == nil {
		t.Fatal("plain text must not pass as a clearsigned document")
	}
}

func TestPublicKeyIsUsable(t *testing.T) {
	ctx := context.Background()
	signer, _ := testSigner(t)

	// The publisher serves this key for clients to import, so what it returns
	// must itself verify signatures from the same signer.
	exported, err := signer.PublicKey(ctx)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if !bytes.HasPrefix(exported, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		t.Fatalf("not an armored public key:\n%s", exported)
	}

	doc := []byte("Origin: HashiCorp\n")
	sig, err := signer.DetachSign(ctx, doc)
	if err != nil {
		t.Fatalf("DetachSign: %v", err)
	}
	if err := Verify(exported, doc, sig); err != nil {
		t.Fatalf("the exported public key does not verify our own signature: %v", err)
	}
}

func TestKeyIsFetchedOnce(t *testing.T) {
	// On Lambda the key is fetched once per container, not once per publish.
	ctx := context.Background()
	source, _, err := GenerateTestKey("Fixture", "fixtures@example.com")
	if err != nil {
		t.Fatalf("GenerateTestKey: %v", err)
	}

	counting := &countingKeySource{inner: source}
	signer := NewPGPSigner(counting)

	for range 3 {
		if _, err := signer.DetachSign(ctx, []byte("doc")); err != nil {
			t.Fatalf("DetachSign: %v", err)
		}
	}
	if counting.calls != 1 {
		t.Errorf("fetched the key %d times, want 1", counting.calls)
	}
}

func TestLoadFailureIsReportedEveryTime(t *testing.T) {
	// A cached failure must keep failing rather than silently publishing
	// unsigned metadata later.
	ctx := context.Background()
	signer := NewPGPSigner(StaticKeySource{Armored: []byte("not a key")})

	for range 2 {
		if _, err := signer.DetachSign(ctx, []byte("doc")); err == nil {
			t.Fatal("expected an error for an unreadable key")
		}
	}
}

func TestRejectsPublicKeyAsSigningKey(t *testing.T) {
	_, public, err := GenerateTestKey("Fixture", "fixtures@example.com")
	if err != nil {
		t.Fatalf("GenerateTestKey: %v", err)
	}

	signer := NewPGPSigner(StaticKeySource{Armored: public})
	_, err = signer.DetachSign(context.Background(), []byte("doc"))
	if err == nil {
		t.Fatal("expected an error when the secret holds a public key")
	}
	if !strings.Contains(err.Error(), "public key") {
		t.Errorf("error = %v, want it to say the secret holds a public key", err)
	}
}

type countingKeySource struct {
	inner KeySource
	calls int
}

func (c *countingKeySource) PrivateKey(ctx context.Context) ([]byte, string, error) {
	c.calls++
	return c.inner.PrivateKey(ctx)
}
