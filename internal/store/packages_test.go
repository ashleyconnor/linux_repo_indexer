package store

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/deb"
	rpmparse "github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta/rpm"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// These tests cover encoding, the blob-overflow decision and how requests are
// shaped. They deliberately do not fake DynamoDB's condition expressions or
// atomic ADD: a fake would only prove that our idea of those semantics is
// self-consistent, which is exactly the thing worth testing against the real
// service. The lease and generation behaviour is covered by the LocalStack
// integration test.

// fakeDynamo records requests and replays canned responses.
type fakeDynamo struct {
	items    map[string]map[string]types.AttributeValue // pkgkey -> item
	pages    [][]map[string]types.AttributeValue
	queries  []*dynamodb.QueryInput
	puts     []*dynamodb.PutItemInput
	deletes  []*dynamodb.DeleteItemInput
	queryPos int
}

func newFakeDynamo() *fakeDynamo {
	return &fakeDynamo{items: map[string]map[string]types.AttributeValue{}}
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.puts = append(f.puts, in)
	key := in.Item["filename"].(*types.AttributeValueMemberS).Value
	f.items[key] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeDynamo) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.deletes = append(f.deletes, in)
	return &dynamodb.DeleteItemOutput{}, nil
}

func (f *fakeDynamo) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queries = append(f.queries, in)

	if f.pages == nil {
		// Default: everything written so far, in one page.
		var items []map[string]types.AttributeValue
		for _, item := range f.items {
			items = append(items, item)
		}
		return &dynamodb.QueryOutput{Items: items}, nil
	}

	page := f.pages[f.queryPos]
	f.queryPos++
	out := &dynamodb.QueryOutput{Items: page}
	if f.queryPos < len(f.pages) {
		out.LastEvaluatedKey = map[string]types.AttributeValue{
			"filename": &types.AttributeValueMemberS{Value: "cursor"},
		}
	}
	return out, nil
}

func (f *fakeDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDynamo) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

// fakeBlobs is an in-memory BlobStore.
type fakeBlobs struct {
	objects map[string][]byte
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{objects: map[string][]byte{}} }

func (f *fakeBlobs) PutBlob(_ context.Context, key string, body []byte) error {
	f.objects[key] = body
	return nil
}

func (f *fakeBlobs) GetBlob(_ context.Context, key string) ([]byte, error) {
	body, ok := f.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return body, nil
}

func loadDeb(t *testing.T, name string) *pkgmeta.Package {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testdata/packages/deb", name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	p, err := deb.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	d := index.DigestsOf(raw)
	p.S3Key = "pool/amd64/main/" + name
	p.Filename = p.S3Key
	p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
	p.UpdatedAt = time.Unix(1785988475, 0).UTC()
	return p
}

func loadRPM(t *testing.T, name string) *pkgmeta.Package {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../testdata/packages/rpm", name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	p, err := rpmparse.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	d := index.DigestsOf(raw)
	p.S3Key = "RHEL/9/x86_64/stable/" + name
	p.Filename = name
	p.Size, p.MD5, p.SHA1, p.SHA256 = d.Size, d.MD5, d.SHA1, d.SHA256
	p.UpdatedAt = time.Unix(1785988475, 0).UTC()
	return p
}

func TestPutListRoundTripDeb(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)
	scope := repoconfig.DebPoolScope("main", "amd64")

	want := loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")
	if err := s.Put(ctx, scope, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d packages, want 1", len(got))
	}

	// The control paragraph is what the Packages file is rendered from, so it
	// must survive storage exactly, field order included.
	if got[0].Deb == nil || got[0].Deb.Control == nil {
		t.Fatal("control paragraph did not survive the round trip")
	}
	if g, w := got[0].Deb.Control.String(), want.Deb.Control.String(); g != w {
		t.Errorf("control paragraph changed:\n%s\nwant\n%s", g, w)
	}
	for _, f := range []struct {
		name      string
		got, want string
	}{
		{"Name", got[0].Name, want.Name},
		{"Version", got[0].Version, want.Version},
		{"Filename", got[0].Filename, want.Filename},
		{"SHA256", got[0].SHA256, want.SHA256},
		{"Description", got[0].Description, want.Description},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if got[0].Size != want.Size {
		t.Errorf("Size = %d, want %d", got[0].Size, want.Size)
	}
}

func TestPutListRoundTripRPM(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)
	scope := repoconfig.RPMTreeScope("RHEL", "9", "x86_64", "stable")

	want := loadRPM(t, "indexer-fixture-1.0.0-1.x86_64.rpm")
	if err := s.Put(ctx, scope, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d packages, want 1", len(got))
	}

	d := got[0].RPM
	if d == nil {
		t.Fatal("rpm detail did not survive the round trip")
	}
	if len(d.Files) != len(want.RPM.Files) {
		t.Errorf("got %d files, want %d", len(d.Files), len(want.RPM.Files))
	}
	if len(d.Changelog) != len(want.RPM.Changelog) {
		t.Errorf("got %d changelog entries, want %d", len(d.Changelog), len(want.RPM.Changelog))
	}
	if len(d.Provides) != len(want.RPM.Provides) {
		t.Errorf("got %d provides, want %d", len(d.Provides), len(want.RPM.Provides))
	}
	// Header range feeds <rpm:header-range>, so it has to be preserved exactly.
	if d.HeaderStart != want.RPM.HeaderStart || d.HeaderEnd != want.RPM.HeaderEnd {
		t.Errorf("header range = %d-%d, want %d-%d", d.HeaderStart, d.HeaderEnd, want.RPM.HeaderStart, want.RPM.HeaderEnd)
	}
	if got[0].Epoch != want.Epoch {
		t.Errorf("Epoch = %d, want %d", got[0].Epoch, want.Epoch)
	}
}

func TestPutWritesQueryableAttributes(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)
	scope := repoconfig.DebPoolScope("main", "amd64")

	pkg := loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")
	if err := s.Put(ctx, scope, pkg); err != nil {
		t.Fatalf("Put: %v", err)
	}

	item := dyn.puts[0].Item
	for _, tt := range []struct{ attr, want string }{
		{"scope", "deb/main/amd64"},
		{"filename", "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"},
		{"name", "indexer-fixture"},
		{"evr", "1.0.0-1"},
		{"arch", "amd64"},
		{"s3key", "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"},
	} {
		v, ok := item[tt.attr].(*types.AttributeValueMemberS)
		if !ok {
			t.Errorf("attribute %q is missing or not a string", tt.attr)
			continue
		}
		if v.Value != tt.want {
			t.Errorf("%s = %q, want %q", tt.attr, v.Value, tt.want)
		}
	}
	if _, ok := item["body"].(*types.AttributeValueMemberB); !ok {
		t.Error("body should be stored inline as binary")
	}
	if _, ok := item["blobKey"]; ok {
		t.Error("a small record should not spill to a blob")
	}
}

func TestPutIsIdempotent(t *testing.T) {
	// SQS delivers at least once, so a replayed ingest must leave the scope
	// with exactly one record for the package, not two.
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)
	scope := repoconfig.DebPoolScope("main", "amd64")

	pkg := loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")
	for range 3 {
		if err := s.Put(ctx, scope, pkg); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, err := s.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d records after 3 identical puts, want 1", len(got))
	}
}

func TestPutSpillsOversizedRecordToBlob(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamo()
	blobs := newFakeBlobs()
	s := NewPackageStore(dyn, "packages", blobs)
	scope := repoconfig.RPMTreeScope("RHEL", "9", "x86_64", "stable")

	// A package with a kernel-sized file list.
	pkg := loadRPM(t, "indexer-fixture-1.0.0-1.x86_64.rpm")
	pkg.RPM.Files = append(pkg.RPM.Files, hugeFileList(t)...)

	if err := s.Put(ctx, scope, pkg); err != nil {
		t.Fatalf("Put: %v", err)
	}

	item := dyn.puts[0].Item
	blobKey, ok := item["blobKey"].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatal("an oversized record should carry a blobKey")
	}
	if !strings.HasPrefix(blobKey.Value, ".meta/pkgblob/") {
		t.Errorf("blobKey = %q, want it under .meta/pkgblob/", blobKey.Value)
	}
	if _, ok := item["body"]; ok {
		t.Error("an oversized record should not also be stored inline")
	}
	if len(blobs.objects) != 1 {
		t.Fatalf("got %d blobs, want 1", len(blobs.objects))
	}

	// And it must read back whole.
	got, err := s.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d packages, want 1", len(got))
	}
	if len(got[0].RPM.Files) != len(pkg.RPM.Files) {
		t.Errorf("got %d files back, want %d", len(got[0].RPM.Files), len(pkg.RPM.Files))
	}
}

func TestPutFailsLoudlyWhenOversizedAndNoBlobStore(t *testing.T) {
	ctx := context.Background()
	s := NewPackageStore(newFakeDynamo(), "packages", nil)

	pkg := loadRPM(t, "indexer-fixture-1.0.0-1.x86_64.rpm")
	pkg.RPM.Files = append(pkg.RPM.Files, hugeFileList(t)...)

	err := s.Put(ctx, repoconfig.RPMTreeScope("RHEL", "9", "x86_64", "stable"), pkg)
	if err == nil {
		t.Fatal("expected an error rather than a silently truncated record")
	}
	if !strings.Contains(err.Error(), "blob store") {
		t.Errorf("error = %v, want it to explain the missing blob store", err)
	}
}

func TestListFollowsPagination(t *testing.T) {
	// A scope holds thousands of packages, well past one Query page.
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)
	scope := repoconfig.DebPoolScope("main", "amd64")

	pkg := loadDeb(t, "indexer-fixture_1.0.0-1_amd64.deb")
	body, err := encodeRecord(pkg)
	if err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	item, err := attributevalue.MarshalMap(record{
		Scope: scope.String(), Filename: pkg.Filename, Body: body,
	})
	if err != nil {
		t.Fatalf("MarshalMap: %v", err)
	}

	dyn.pages = [][]map[string]types.AttributeValue{
		{item, item},
		{item},
		{item, item, item},
	}

	got, err := s.List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 6 {
		t.Errorf("got %d packages across 3 pages, want 6", len(got))
	}
	if len(dyn.queries) != 3 {
		t.Errorf("made %d queries, want 3", len(dyn.queries))
	}
	if dyn.queries[1].ExclusiveStartKey == nil {
		t.Error("the second query should continue from the first page's cursor")
	}
}

func TestDeleteIsKeyedByObject(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeDynamo()
	s := NewPackageStore(dyn, "packages", nil)

	scope := repoconfig.DebPoolScope("main", "amd64")
	if err := s.Delete(ctx, scope, "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	key := dyn.deletes[0].Key
	if got := key["scope"].(*types.AttributeValueMemberS).Value; got != "deb/main/amd64" {
		t.Errorf("scope key = %q", got)
	}
	if got := key["filename"].(*types.AttributeValueMemberS).Value; got != "pool/amd64/main/indexer-fixture_1.0.0-1_amd64.deb" {
		t.Errorf("filename = %q", got)
	}
}

func TestStateDirty(t *testing.T) {
	if (State{Generation: 5, PublishedGeneration: 5}).Dirty() {
		t.Error("a scope published at its current generation is clean")
	}
	if !(State{Generation: 6, PublishedGeneration: 5}).Dirty() {
		t.Error("a scope with unpublished changes is dirty")
	}
	if !(State{Generation: 1}).Dirty() {
		t.Error("a scope that has never been published is dirty")
	}
}

// hugeFileList builds a file list big enough to exceed the inline item limit
// even after gzip, which is what a kernel package looks like to the store.
//
// The names come from a seeded PRNG rather than a counter: a predictable
// sequence compresses away to almost nothing and the record would stay inline,
// so the test would pass without exercising the spill at all.
func hugeFileList(t *testing.T) []pkgmeta.File {
	t.Helper()

	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	rng := rand.New(rand.NewSource(1))

	files := make([]pkgmeta.File, 0, 200_000)
	name := make([]byte, 24)
	for range cap(files) {
		for j := range name {
			name[j] = alphabet[rng.Intn(len(alphabet))]
		}
		files = append(files, pkgmeta.File{
			Path: "/usr/lib/modules/6.1.0/kernel/drivers/" + string(name) + ".ko.xz",
		})
	}
	return files
}
