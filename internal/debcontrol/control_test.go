package debcontrol

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseSimpleParagraph(t *testing.T) {
	p, err := Parse("Package: nginx\nVersion: 1.28.0\nArchitecture: amd64\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := p.Get("Package"), "nginx"; got != want {
		t.Errorf("Package = %q, want %q", got, want)
	}
	if got, want := p.Get("version"), "1.28.0"; got != want {
		t.Errorf("Get is case-insensitive: got %q, want %q", got, want)
	}
	if got := p.Get("Depends"); got != "" {
		t.Errorf("missing field = %q, want empty", got)
	}
	if got, want := len(p.Fields), 3; got != want {
		t.Errorf("len(Fields) = %d, want %d", got, want)
	}
}

func TestParsePreservesFieldOrder(t *testing.T) {
	// Artifactory emits control fields in the order the package declared them,
	// which differs between packages. Round-tripping must not reorder them.
	p, err := Parse("Package: boundary\nVersion: 0.1.0\nLicense: MPL-2.0\nVendor: HashiCorp\nArchitecture: amd64\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"Package", "Version", "License", "Vendor", "Architecture"}
	for i, f := range p.Fields {
		if f.Name != want[i] {
			t.Fatalf("field %d = %q, want %q", i, f.Name, want[i])
		}
	}
}

func TestParseContinuationLines(t *testing.T) {
	const in = "Package: foo\n" +
		"Description: short summary\n" +
		" first body line\n" +
		" .\n" +
		"  indented body line\n" +
		"Architecture: amd64\n"

	p, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "short summary\nfirst body line\n.\n indented body line"
	if got := p.Get("Description"); got != want {
		t.Errorf("Description = %q, want %q", got, want)
	}
	if got, want := p.Get("Architecture"), "amd64"; got != want {
		t.Errorf("field after continuation = %q, want %q", got, want)
	}
}

func TestParseEmptyValue(t *testing.T) {
	// The live HashiCorp index contains "Section: " with no value.
	p, err := Parse("Package: athena-cli\nSection: \nPriority: optional\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := p.Get("Section"); got != "" {
		t.Errorf("Section = %q, want empty", got)
	}
	if len(p.Fields) != 3 {
		t.Errorf("empty-valued field must be retained, got %d fields", len(p.Fields))
	}
}

func TestParseRejectsMissingColon(t *testing.T) {
	if _, err := Parse("Package: foo\nthis is not a field\n"); err == nil {
		t.Fatal("expected an error for a line with no colon")
	}
}

func TestParseRejectsLeadingContinuation(t *testing.T) {
	if _, err := Parse(" orphaned continuation\nPackage: foo\n"); err == nil {
		t.Fatal("expected an error for a continuation with no preceding field")
	}
}

func TestReaderMultipleParagraphs(t *testing.T) {
	const in = "\n\nPackage: a\nVersion: 1\n\n\nPackage: b\nVersion: 2\n\n"

	got, err := ReadAll(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(got))
	}
	if got[0].Get("Package") != "a" || got[1].Get("Package") != "b" {
		t.Errorf("got packages %q and %q, want a and b", got[0].Get("Package"), got[1].Get("Package"))
	}
}

func TestReaderReturnsEOF(t *testing.T) {
	r := NewReader(strings.NewReader("Package: a\n"))
	if _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Next err = %v, want io.EOF", err)
	}
}

func TestParseStripsCarriageReturns(t *testing.T) {
	p, err := Parse("Package: foo\r\nVersion: 1.0\r\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := p.Get("Version"), "1.0"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	const in = "Package: foo\n" +
		"Version: 1.2.3\n" +
		"Section: \n" +
		"Description: short summary\n" +
		" first body line\n" +
		" .\n" +
		"  indented body line\n" +
		"Architecture: amd64\n" +
		"\n"

	p, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := p.String(); got != in {
		t.Errorf("round trip mismatch:\n got %q\nwant %q", got, in)
	}
}

func TestSetReplacesInPlace(t *testing.T) {
	p, _ := Parse("Package: foo\nVersion: 1\nArchitecture: amd64\n")
	p.Set("Version", "2")
	if got, want := p.Get("Version"), "2"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	if got, want := p.Fields[1].Name, "Version"; got != want {
		t.Errorf("Set moved the field: index 1 is %q, want %q", got, want)
	}
	if len(p.Fields) != 3 {
		t.Errorf("Set appended instead of replacing: %d fields", len(p.Fields))
	}
}

func TestSetAppendsNewField(t *testing.T) {
	p, _ := Parse("Package: foo\n")
	p.Set("Filename", "pool/amd64/main/foo.deb")
	if got, want := len(p.Fields), 2; got != want {
		t.Fatalf("len(Fields) = %d, want %d", got, want)
	}
	if got, want := p.Fields[1].Name, "Filename"; got != want {
		t.Errorf("appended field = %q, want %q", got, want)
	}
}

func TestDelete(t *testing.T) {
	p, _ := Parse("Package: foo\nMD5sum: abc\nVersion: 1\n")
	p.Delete("md5sum")
	if got := p.Get("MD5sum"); got != "" {
		t.Errorf("MD5sum = %q, want empty after Delete", got)
	}
	if got, want := len(p.Fields), 2; got != want {
		t.Errorf("len(Fields) = %d, want %d", got, want)
	}
}
