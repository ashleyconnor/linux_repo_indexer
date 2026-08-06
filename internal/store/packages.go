package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// A record is one row in the packages table.
//
// The package itself is stored as one gzipped JSON blob rather than as mapped
// attributes. Nothing ever queries an inner field — the publisher always reads
// a whole scope — so mapping them would buy nothing and would make every model
// change a migration. Compression also matters: an RPM's file list is the bulk
// of a record and compresses roughly tenfold.
type record struct {
	Scope string `dynamodbav:"scope"`

	// Filename is the sort key: the object's path as the index records it,
	// which is the full key for a .deb and the base name for a .rpm.
	//
	// It is deliberately the object rather than the package's name-version-
	// architecture. An ObjectRemoved event carries only the S3 key, and an
	// RPM's epoch does not appear in its filename, so a NEVRA-keyed row could
	// not be located to delete. Keying by object makes ingest and removal
	// exactly symmetric, and both idempotent under SQS redelivery.
	Filename string `dynamodbav:"filename"`

	// Queryable copies, for operators reading the table directly.
	Name         string `dynamodbav:"name"`
	EVR          string `dynamodbav:"evr"`
	Architecture string `dynamodbav:"arch"`
	S3Key        string `dynamodbav:"s3key"`
	UpdatedAt    int64  `dynamodbav:"updatedAt"`

	// Body holds the gzipped JSON, unless it was too large for an item, in
	// which case BlobKey points at it in S3 instead.
	Body    []byte `dynamodbav:"body,omitempty"`
	BlobKey string `dynamodbav:"blobKey,omitempty"`
}

// maxInlineBody caps how much compressed JSON is stored in the item itself.
//
// DynamoDB's hard limit is 400 KB for the whole item; staying well under it
// leaves room for the other attributes. A package big enough to exceed this —
// a kernel, with tens of thousands of files — spills to S3 rather than failing
// to index.
const maxInlineBody = 300 << 10

// blobPrefix is where oversized package records are kept in the bucket. The
// leading dot keeps them out of the way of the published repository tree.
const blobPrefix = ".meta/pkgblob"

// A BlobStore holds package records too large for a DynamoDB item.
type BlobStore interface {
	PutBlob(ctx context.Context, key string, body []byte) error
	GetBlob(ctx context.Context, key string) ([]byte, error)
}

// PackageStore reads and writes package metadata.
type PackageStore struct {
	client DynamoAPI
	table  string
	blobs  BlobStore
}

// NewPackageStore returns a store over the given packages table. blobs may be
// nil, in which case an oversized record is an error rather than a spill.
func NewPackageStore(client DynamoAPI, table string, blobs BlobStore) *PackageStore {
	return &PackageStore{client: client, table: table, blobs: blobs}
}

// Put writes a package record, replacing any previous record for the same
// object.
//
// It is idempotent, which matters because SQS delivers at least once: an
// ingest replayed after a partial batch failure must not corrupt the scope.
func (s *PackageStore) Put(ctx context.Context, scope repoconfig.Scope, pkg *pkgmeta.Package) error {
	body, err := encodeRecord(pkg)
	if err != nil {
		return err
	}

	if pkg.Filename == "" {
		return fmt.Errorf("store: %s has no filename, so it has no identity within %s", pkg.String(), scope)
	}

	r := record{
		Scope:        scope.String(),
		Filename:     pkg.Filename,
		Name:         pkg.Name,
		EVR:          pkg.EVR(),
		Architecture: pkg.Architecture,
		S3Key:        pkg.S3Key,
		UpdatedAt:    pkg.UpdatedAt.Unix(),
	}

	if len(body) <= maxInlineBody {
		r.Body = body
	} else {
		if s.blobs == nil {
			return fmt.Errorf("store: %s is %d bytes compressed, over the %d inline limit, and no blob store is configured",
				pkg.String(), len(body), maxInlineBody)
		}
		r.BlobKey = path.Join(blobPrefix, index.SHA256Hex(body)+".json.gz")
		if err := s.blobs.PutBlob(ctx, r.BlobKey, body); err != nil {
			return fmt.Errorf("store: spilling %s to %s: %w", pkg.String(), r.BlobKey, err)
		}
	}

	item, err := attributevalue.MarshalMap(r)
	if err != nil {
		return fmt.Errorf("store: encoding record for %s: %w", pkg.String(), err)
	}
	if _, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.table),
		Item:      item,
	}); err != nil {
		return fmt.Errorf("store: writing %s to %s: %w", pkg.String(), scope, err)
	}
	return nil
}

// Delete removes the record for one object. Deleting something already absent
// is not an error, so a replayed S3 removal event is harmless.
func (s *PackageStore) Delete(ctx context.Context, scope repoconfig.Scope, filename string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"scope":    &types.AttributeValueMemberS{Value: scope.String()},
			"filename": &types.AttributeValueMemberS{Value: filename},
		},
	})
	if err != nil {
		return fmt.Errorf("store: deleting %s from %s: %w", filename, scope, err)
	}
	return nil
}

// List returns every package in a scope, following pagination.
//
// This is the read the publisher makes before generating an index: one
// sequential pass over structured metadata, in place of opening thousands of
// package files.
func (s *PackageStore) List(ctx context.Context, scope repoconfig.Scope) ([]pkgmeta.Package, error) {
	var (
		out       []pkgmeta.Package
		lastKey   map[string]types.AttributeValue
		pageCount int
	)

	for {
		page, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("#scope = :scope"),
			ExpressionAttributeNames: map[string]string{
				"#scope": "scope",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":scope": &types.AttributeValueMemberS{Value: scope.String()},
			},
			ExclusiveStartKey: lastKey,
		})
		if err != nil {
			return nil, fmt.Errorf("store: listing %s (page %d): %w", scope, pageCount, err)
		}
		pageCount++

		for _, item := range page.Items {
			var r record
			if err := attributevalue.UnmarshalMap(item, &r); err != nil {
				return nil, fmt.Errorf("store: decoding a record in %s: %w", scope, err)
			}

			body := r.Body
			if r.BlobKey != "" {
				if s.blobs == nil {
					return nil, fmt.Errorf("store: %s in %s is stored at %s but no blob store is configured", r.Filename, scope, r.BlobKey)
				}
				body, err = s.blobs.GetBlob(ctx, r.BlobKey)
				if err != nil {
					return nil, fmt.Errorf("store: reading %s for %s: %w", r.BlobKey, r.Filename, err)
				}
			}

			pkg, err := decodeRecord(body)
			if err != nil {
				return nil, fmt.Errorf("store: decoding %s in %s: %w", r.Filename, scope, err)
			}
			out = append(out, *pkg)
		}

		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		lastKey = page.LastEvaluatedKey
	}
}

func encodeRecord(pkg *pkgmeta.Package) ([]byte, error) {
	raw, err := json.Marshal(pkg)
	if err != nil {
		return nil, fmt.Errorf("store: encoding %s: %w", pkg.String(), err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("store: compressing %s: %w", pkg.String(), err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("store: compressing %s: %w", pkg.String(), err)
	}
	return buf.Bytes(), nil
}

func decodeRecord(body []byte) (*pkgmeta.Package, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	defer zr.Close()

	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}

	var pkg pkgmeta.Package
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("decoding json: %w", err)
	}
	return &pkg, nil
}
