package pkgmeta

import "testing"

func TestEVRAndKey(t *testing.T) {
	tests := []struct {
		name    string
		pkg     Package
		wantEVR string
		wantKey string
	}{
		{
			name:    "rpm always carries an explicit epoch",
			pkg:     Package{Format: FormatRPM, Name: "consul", Version: "1.21.0", Release: "1", Architecture: "x86_64"},
			wantEVR: "0:1.21.0-1",
			wantKey: "consul#0:1.21.0-1#x86_64",
		},
		{
			name:    "rpm with a non-zero epoch",
			pkg:     Package{Format: FormatRPM, Name: "vault", Epoch: 2, Version: "1.17.2", Release: "1", Architecture: "aarch64"},
			wantEVR: "2:1.17.2-1",
			wantKey: "vault#2:1.17.2-1#aarch64",
		},
		{
			name:    "deb version is used verbatim",
			pkg:     Package{Format: FormatDeb, Name: "nomad", Version: "1.9.3-1", Architecture: "amd64"},
			wantEVR: "1.9.3-1",
			wantKey: "nomad#1.9.3-1#amd64",
		},
		{
			name:    "deb epoch stays inline in the version",
			pkg:     Package{Format: FormatDeb, Name: "boundary", Version: "1:0.1.0", Architecture: "amd64"},
			wantEVR: "1:0.1.0",
			wantKey: "boundary#1:0.1.0#amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pkg.EVR(); got != tt.wantEVR {
				t.Errorf("EVR() = %q, want %q", got, tt.wantEVR)
			}
			if got := tt.pkg.Key(); got != tt.wantKey {
				t.Errorf("Key() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestEVRDistinguishesEpochs(t *testing.T) {
	// "1.0" with epoch 1 and "1:1.0" with epoch 0 must not share a sort key.
	a := Package{Format: FormatRPM, Name: "x", Epoch: 1, Version: "1.0", Release: "1", Architecture: "x86_64"}
	b := Package{Format: FormatRPM, Name: "x", Epoch: 0, Version: "1.0", Release: "1", Architecture: "x86_64"}
	if a.Key() == b.Key() {
		t.Fatalf("keys collide: %q", a.Key())
	}
}

func TestNEVRA(t *testing.T) {
	p := Package{Format: FormatRPM, Name: "consul", Version: "1.21.0", Release: "1", Architecture: "x86_64"}
	if got, want := p.NEVRA(), "consul-1.21.0-1.x86_64"; got != want {
		t.Errorf("NEVRA() = %q, want %q", got, want)
	}
	p.Epoch = 3
	if got, want := p.NEVRA(), "consul-3:1.21.0-1.x86_64"; got != want {
		t.Errorf("NEVRA() with epoch = %q, want %q", got, want)
	}
}

func TestDependencyComparison(t *testing.T) {
	tests := []struct {
		flags int
		want  string
	}{
		{SenseAny, ""},
		{SenseLess, "LT"},
		{SenseLess | SenseEqual, "LE"},
		{SenseGreater, "GT"},
		{SenseGreater | SenseEqual, "GE"},
		{SenseEqual, "EQ"},
		{SenseEqual | SensePreReq, "EQ"}, // unrelated bits must not confuse it
	}
	for _, tt := range tests {
		d := Dependency{Name: "libc", Flags: tt.flags}
		if got := d.Comparison(); got != tt.want {
			t.Errorf("flags %#x: Comparison() = %q, want %q", tt.flags, got, tt.want)
		}
	}
}

func TestDependencyIsPre(t *testing.T) {
	if (Dependency{Flags: SenseEqual}).IsPre() {
		t.Error("a plain versioned dependency is not a pre-requirement")
	}
	for _, f := range []int{SensePreReq, SenseScriptPre, SenseScriptPost, SenseScriptPreUn, SenseScriptPostUn} {
		if !(Dependency{Flags: f}).IsPre() {
			t.Errorf("flag %#x should be a pre-requirement", f)
		}
	}
}

func TestDependencyIsRPMLib(t *testing.T) {
	if !(Dependency{Name: "rpmlib(CompressedFileNames)"}).IsRPMLib() {
		t.Error("rpmlib(...) dependencies must be detected by name")
	}
	if !(Dependency{Name: "anything", Flags: SenseRPMLib}).IsRPMLib() {
		t.Error("the RPMSENSE_RPMLIB flag must be detected")
	}
	if (Dependency{Name: "openssl"}).IsRPMLib() {
		t.Error("an ordinary dependency is not an rpmlib feature")
	}
}

func TestIsPrimaryFile(t *testing.T) {
	primary := []string{
		"/usr/bin/consul",
		"/usr/sbin/vault",
		"/etc/consul.d",
		"/etc/consul.d/consul.hcl",
		"/usr/lib/sendmail",
		"/opt/vendor/bin/tool",
	}
	for _, p := range primary {
		if !IsPrimaryFile(p) {
			t.Errorf("%q should be a primary file", p)
		}
	}

	other := []string{
		"/usr/share/doc/consul/README",
		"/usr/lib/systemd/system/consul.service",
		"/var/lib/consul",
	}
	for _, p := range other {
		if IsPrimaryFile(p) {
			t.Errorf("%q should not be a primary file", p)
		}
	}
}

func TestPrimaryFilesFiltersFileList(t *testing.T) {
	d := &RPMDetail{Files: []File{
		{Path: "/usr/bin/consul"},
		{Path: "/usr/share/doc/consul", Type: FileTypeDir},
		{Path: "/etc/consul.d", Type: FileTypeDir},
		{Path: "/var/log/consul.log", Type: FileTypeGhost},
	}}
	got := d.PrimaryFiles()
	if len(got) != 2 {
		t.Fatalf("got %d primary files, want 2: %+v", len(got), got)
	}
	if got[0].Path != "/usr/bin/consul" || got[1].Path != "/etc/consul.d" {
		t.Errorf("unexpected primary files: %+v", got)
	}
	if got[1].Type != FileTypeDir {
		t.Errorf("file type must be preserved, got %q", got[1].Type)
	}
}
