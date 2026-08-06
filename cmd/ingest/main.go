// Command ingest is the Lambda that turns S3 object events into package
// records.
//
// It is driven by an SQS queue rather than by S3 directly, so a burst of
// uploads is buffered and a failing message is retried and then parked in a
// dead-letter queue instead of being lost.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/ashleyconnor/linux-repo-indexer/internal/awsx"
	"github.com/ashleyconnor/linux-repo-indexer/internal/ingest"
)

func main() {
	log := awsx.Logger()

	env, err := awsx.LoadEnv()
	if err != nil {
		log.Error("startup", "error", err)
		panic(err)
	}

	clients, err := awsx.Connect(context.Background(), env)
	if err != nil {
		log.Error("startup", "error", err)
		panic(err)
	}

	h := &handler{clients: clients, log: log}
	lambda.Start(h.handle)
}

type handler struct {
	clients *awsx.Clients
	log     *slog.Logger
}

// handle processes one SQS batch.
//
// Failures are reported per message rather than for the batch. Without that, a
// single unparseable package would send its nine healthy neighbours round the
// retry loop with it, and eventually to the dead-letter queue.
func (h *handler) handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	cfg, err := h.clients.LoadRepoConfig(ctx)
	if err != nil {
		// Nothing can be classified without the config, so fail the whole
		// batch and let SQS retry.
		return events.SQSEventResponse{}, err
	}

	ing := &ingest.Ingester{
		Config:       cfg,
		Objects:      h.clients.S3,
		Packages:     h.clients.Packages,
		State:        h.clients.State,
		Queue:        h.clients.Queue,
		PublishDelay: h.clients.Env.PublishDelay,
		Log:          h.log,
	}

	var failures []events.SQSBatchItemFailure

	for _, message := range event.Records {
		if err := h.handleMessage(ctx, ing, message.Body); err != nil {
			h.log.Error("ingest failed",
				"messageId", message.MessageId,
				"error", err)
			failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: message.MessageId})
		}
	}

	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

func (h *handler) handleMessage(ctx context.Context, ing *ingest.Ingester, body string) error {
	var event events.S3Event
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		return fmt.Errorf("ingest: decoding s3 event: %w", err)
	}

	// S3 sends a test event when a notification is first configured; it has no
	// records and is not a failure.
	for _, record := range event.Records {
		key, err := objectKey(record.S3.Object.Key)
		if err != nil {
			return err
		}

		switch {
		case strings.HasPrefix(record.EventName, "ObjectCreated:"):
			if err := ing.Created(ctx, key); err != nil {
				return err
			}
		case strings.HasPrefix(record.EventName, "ObjectRemoved:"):
			if err := ing.Removed(ctx, key); err != nil {
				return err
			}
		default:
			h.log.Debug("ignoring event", "event", record.EventName, "key", key)
		}
	}
	return nil
}

// objectKey undoes the URL encoding S3 applies to keys in event notifications.
// A package filename containing a plus sign — "1.9.5+fips1402" is a real
// example from the live repository — arrives as "%2B" and would otherwise be
// looked up under a key that does not exist.
func objectKey(encoded string) (string, error) {
	key, err := url.QueryUnescape(strings.ReplaceAll(encoded, "+", "%20"))
	if err != nil {
		return "", fmt.Errorf("ingest: decoding object key %q: %w", encoded, err)
	}
	return key, nil
}
