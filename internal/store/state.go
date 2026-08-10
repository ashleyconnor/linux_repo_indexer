// Package store persists package metadata and per-scope publish state in
// DynamoDB.
//
// Two tables: packages holds one item per package file, partitioned by scope;
// repo_state holds one item per scope carrying the generation counter, the
// publish lease and the digests of the last published index files.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/ashleyconnor/linux-repo-indexer/internal/index"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// DynamoAPI is the slice of the DynamoDB client this package uses. Narrowing
// it keeps the store unit-testable without standing up a database.
type DynamoAPI interface {
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// ErrLeaseHeld is returned when another publisher holds a scope's lease. It is
// not a failure: the caller should return its message to the queue and let the
// current holder finish. The holder cannot be assumed to cover the caller's
// work, because it captured its generation before the caller's message was
// sent.
var ErrLeaseHeld = errors.New("store: scope lease is held by another publisher")

// State is one scope's publish state.
type State struct {
	Scope string `dynamodbav:"scope"`

	// Generation increments on every package added or removed. A scope is
	// dirty while Generation differs from PublishedGeneration.
	Generation          int64 `dynamodbav:"generation"`
	PublishedGeneration int64 `dynamodbav:"publishedGeneration"`

	LeaseOwner  string `dynamodbav:"leaseOwner,omitempty"`
	LeaseExpiry int64  `dynamodbav:"leaseExpiry,omitempty"` // unix seconds

	LastPublishedAt int64 `dynamodbav:"lastPublishedAt,omitempty"`

	// Artifacts records what the last publish wrote, so a codename's Release
	// can be assembled from the digests of every component and architecture
	// without re-reading S3.
	Artifacts []PublishedArtifact `dynamodbav:"artifacts,omitempty"`
}

// A PublishedArtifact is one index file as the Release file must cite it.
type PublishedArtifact struct {
	Path    string        `dynamodbav:"path"`
	Digests index.Digests `dynamodbav:"digests"`
}

// Dirty reports whether the scope has changes that have not been published.
func (s State) Dirty() bool { return s.Generation != s.PublishedGeneration }

// StateStore reads and writes per-scope publish state.
type StateStore struct {
	client DynamoAPI
	table  string
}

// NewStateStore returns a store over the given repo_state table.
func NewStateStore(client DynamoAPI, table string) *StateStore {
	return &StateStore{client: client, table: table}
}

// Get returns a scope's state. A scope that has never been touched reads back
// zero-valued rather than missing, because "no packages yet" and "no row yet"
// mean the same thing to the publisher.
func (s *StateStore) Get(ctx context.Context, scope repoconfig.Scope) (State, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            scopeKey(scope),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return State{}, fmt.Errorf("store: reading state for %s: %w", scope, err)
	}
	if out.Item == nil {
		return State{Scope: scope.String()}, nil
	}

	var st State
	if err := attributevalue.UnmarshalMap(out.Item, &st); err != nil {
		return State{}, fmt.Errorf("store: decoding state for %s: %w", scope, err)
	}
	return st, nil
}

// MarkDirty increments a scope's generation and returns the new value.
//
// It is an atomic ADD rather than a read-modify-write, so concurrent ingests
// of different packages into the same scope each register, and none is lost.
func (s *StateStore) MarkDirty(ctx context.Context, scope repoconfig.Scope) (int64, error) {
	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.table),
		Key:              scopeKey(scope),
		UpdateExpression: aws.String("ADD generation :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
		},
		ReturnValues: types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, fmt.Errorf("store: marking %s dirty: %w", scope, err)
	}

	var updated struct {
		Generation int64 `dynamodbav:"generation"`
	}
	if err := attributevalue.UnmarshalMap(out.Attributes, &updated); err != nil {
		return 0, fmt.Errorf("store: decoding generation for %s: %w", scope, err)
	}
	return updated.Generation, nil
}

// AcquireLease claims exclusive publishing rights over a scope until it
// expires.
//
// Without it two publishers can interleave: one reads the package set at
// generation 5, another at generation 7, the second writes its index first,
// and the first then overwrites it with the older content while the state says
// generation 7 is published. The lease makes that impossible, and a TTL means
// a publisher that dies mid-run does not block the scope forever.
func (s *StateStore) AcquireLease(ctx context.Context, scope repoconfig.Scope, owner string, ttl time.Duration, now time.Time) (State, error) {
	expiry := now.Add(ttl).Unix()

	out, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key:       scopeKey(scope),
		UpdateExpression: aws.String(
			"SET leaseOwner = :owner, leaseExpiry = :expiry"),
		ConditionExpression: aws.String(
			"attribute_not_exists(leaseExpiry) OR leaseExpiry < :now OR leaseOwner = :owner"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner":  &types.AttributeValueMemberS{Value: owner},
			":expiry": numberValue(expiry),
			":now":    numberValue(now.Unix()),
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		var failed *types.ConditionalCheckFailedException
		if errors.As(err, &failed) {
			return State{}, fmt.Errorf("%w: %s", ErrLeaseHeld, scope)
		}
		return State{}, fmt.Errorf("store: acquiring lease on %s: %w", scope, err)
	}

	var st State
	if err := attributevalue.UnmarshalMap(out.Attributes, &st); err != nil {
		return State{}, fmt.Errorf("store: decoding state for %s: %w", scope, err)
	}
	return st, nil
}

// ReleaseLease drops the lease, but only if this owner still holds it. A
// publisher that overran its TTL must not evict whoever took over.
func (s *StateStore) ReleaseLease(ctx context.Context, scope repoconfig.Scope, owner string) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           aws.String(s.table),
		Key:                 scopeKey(scope),
		UpdateExpression:    aws.String("REMOVE leaseOwner, leaseExpiry"),
		ConditionExpression: aws.String("leaseOwner = :owner"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":owner": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil {
		var failed *types.ConditionalCheckFailedException
		if errors.As(err, &failed) {
			return nil // someone else owns it now; nothing to release
		}
		return fmt.Errorf("store: releasing lease on %s: %w", scope, err)
	}
	return nil
}

// RecordPublished marks a scope published at the given generation and stores
// the digests of what was written.
//
// The write is conditional on this owner still holding the lease. If it fails,
// the scope simply stays dirty and is republished, which is the safe outcome:
// a stale index is never advertised as current.
func (s *StateStore) RecordPublished(
	ctx context.Context,
	scope repoconfig.Scope,
	owner string,
	generation int64,
	artifacts []PublishedArtifact,
	now time.Time,
) error {
	encoded, err := attributevalue.MarshalList(artifacts)
	if err != nil {
		return fmt.Errorf("store: encoding artifacts for %s: %w", scope, err)
	}

	_, err = s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key:       scopeKey(scope),
		UpdateExpression: aws.String(
			"SET publishedGeneration = :gen, lastPublishedAt = :now, artifacts = :artifacts"),
		ConditionExpression: aws.String("leaseOwner = :owner"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":gen":       numberValue(generation),
			":now":       numberValue(now.Unix()),
			":artifacts": &types.AttributeValueMemberL{Value: encoded},
			":owner":     &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil {
		var failed *types.ConditionalCheckFailedException
		if errors.As(err, &failed) {
			return fmt.Errorf("%w: lost lease on %s before recording generation %d", ErrLeaseHeld, scope, generation)
		}
		return fmt.Errorf("store: recording publish of %s: %w", scope, err)
	}
	return nil
}

func scopeKey(scope repoconfig.Scope) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"scope": &types.AttributeValueMemberS{Value: scope.String()},
	}
}

func numberValue[T int | int64](v T) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: fmt.Sprint(v)}
}
