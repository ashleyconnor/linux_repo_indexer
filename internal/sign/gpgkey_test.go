package sign

import (
	"context"
	"os"
	"testing"
)

func TestSignWithGnuPGGeneratedKey(t *testing.T) {
	priv, err := os.ReadFile("../../testdata/signing/test-key.private.asc")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile("../../testdata/signing/test-key.public.asc")
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte("<repomd><revision>1</revision></repomd>")
	sig, err := NewPGPSigner(StaticKeySource{Armored: priv}).DetachSign(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll("/tmp/gpgdiag2", 0o755)
	os.WriteFile("/tmp/gpgdiag2/pub.asc", pub, 0o644)
	os.WriteFile("/tmp/gpgdiag2/doc", doc, 0o644)
	os.WriteFile("/tmp/gpgdiag2/doc.asc", sig, 0o644)
	if err := Verify(pub, doc, sig); err != nil {
		t.Fatal(err)
	}
}
