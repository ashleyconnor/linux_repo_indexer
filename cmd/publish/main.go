// Command publish is the Lambda that rebuilds repository indexes.
//
// It answers two triggers: SQS messages naming a scope that has just changed,
// and a scheduled sweep that republishes anything left dirty. The sweep is the
// backstop for work that reached the dead-letter queue, which leaves a scope
// dirty with no message pending and which nothing else would notice.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"

	"github.com/ashleyconnor/linux-repo-indexer/internal/awsx"
	"github.com/ashleyconnor/linux-repo-indexer/internal/publish"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
	"github.com/ashleyconnor/linux-repo-indexer/internal/sign"
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

// event is the union of what can arrive: an SQS batch, or a scheduled sweep.
// Decoding into one struct avoids registering two handlers for what is
// otherwise the same work.
type event struct {
	Records []events.SQSMessage `json:"Records"`
	Source  string              `json:"source"`
}

func (h *handler) handle(ctx context.Context, raw json.RawMessage) (events.SQSEventResponse, error) {
	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		return events.SQSEventResponse{}, fmt.Errorf("publish: decoding event: %w", err)
	}

	pub, err := h.publisher(ctx)
	if err != nil {
		return events.SQSEventResponse{}, err
	}

	if len(e.Records) == 0 {
		// A scheduled invocation.
		h.log.Info("sweeping for unpublished scopes")
		return events.SQSEventResponse{}, pub.Sweep(ctx)
	}

	var failures []events.SQSBatchItemFailure
	for _, message := range e.Records {
		if err := h.handleMessage(ctx, pub, message.Body); err != nil {
			h.log.Error("publish failed", "messageId", message.MessageId, "error", err)
			failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: message.MessageId})
		}
	}
	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

func (h *handler) handleMessage(ctx context.Context, pub *publish.Publisher, body string) error {
	var msg awsx.PublishMessage
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		return fmt.Errorf("publish: decoding message: %w", err)
	}

	scope, err := repoconfig.ParseScope(msg.Scope)
	if err != nil {
		// A malformed scope will never succeed, so retrying is pointless;
		// report it and let the message drain rather than cycling to the DLQ.
		h.log.Error("discarding malformed publish message", "scope", msg.Scope, "error", err)
		return nil
	}

	return pub.Publish(ctx, scope)
}

func (h *handler) publisher(ctx context.Context) (*publish.Publisher, error) {
	cfg, err := h.clients.LoadRepoConfig(ctx)
	if err != nil {
		return nil, err
	}

	var signer sign.Signer
	if cfg.Signing.SecretID != "" {
		signer = sign.NewPGPSigner(awsx.SecretsKeySource{
			Client:   h.clients.Secrets,
			SecretID: cfg.Signing.SecretID,
		})
	} else {
		// An unsigned repository is only useful for a test stack, so say so
		// rather than letting it pass unnoticed into production.
		h.log.Warn("no signing key configured; publishing an unsigned repository")
	}

	return &publish.Publisher{
		Config:   cfg,
		Packages: h.clients.Packages,
		State:    h.clients.State,
		Sync:     h.clients.S3,
		Signer:   signer,
		Queue:    h.clients.Queue,
		Owner:    owner(ctx),
		LeaseTTL: h.clients.Env.LeaseTTL,
		Log:      h.log,
	}, nil
}

// owner identifies this invocation in the publish lease, so a log line can
// name whoever is holding a contested scope.
func owner(ctx context.Context) string {
	if lc, ok := lambdacontext.FromContext(ctx); ok {
		return lc.AwsRequestID
	}
	return "local"
}
