package awsx

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

// A PublishMessage asks the publisher to rebuild one scope.
//
// It carries the scope and nothing else. Anything more would be a snapshot of
// state that may already be stale by the time the message is delivered; the
// publisher reads the current generation itself.
type PublishMessage struct {
	Scope string `json:"scope"`
}

// SQSQueue enqueues publish work.
type SQSQueue struct {
	Client   *sqs.Client
	QueueURL string
}

// EnqueuePublish schedules a rebuild of one scope.
//
// The delay is the coalescing window: uploading many packages at once should
// produce one index build, and by the time the first delayed message arrives
// the rest have already been stored. Later duplicates find the scope clean and
// return without writing.
func (q *SQSQueue) EnqueuePublish(ctx context.Context, scope repoconfig.Scope, delay time.Duration) error {
	body, err := json.Marshal(PublishMessage{Scope: scope.String()})
	if err != nil {
		return fmt.Errorf("awsx: encoding publish message: %w", err)
	}

	seconds := int32(delay.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	if seconds > 900 { // SQS maximum
		seconds = 900
	}

	if _, err := q.Client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:     aws.String(q.QueueURL),
		MessageBody:  aws.String(string(body)),
		DelaySeconds: seconds,
	}); err != nil {
		return fmt.Errorf("awsx: enqueuing publish of %s: %w", scope, err)
	}
	return nil
}

// SecretsKeySource reads the OpenPGP signing key from Secrets Manager.
//
// The secret may be either the armored key on its own or a JSON object with
// "private_key" and "passphrase", which is what an operator rotating a
// passphrase-protected key will want.
type SecretsKeySource struct {
	Client   *secretsmanager.Client
	SecretID string
}

// PrivateKey implements sign.KeySource.
func (s SecretsKeySource) PrivateKey(ctx context.Context) ([]byte, string, error) {
	out, err := s.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(s.SecretID),
	})
	if err != nil {
		return nil, "", fmt.Errorf("awsx: reading secret %s: %w", s.SecretID, err)
	}

	raw := aws.ToString(out.SecretString)
	if raw == "" {
		raw = string(out.SecretBinary)
	}
	if raw == "" {
		return nil, "", fmt.Errorf("awsx: secret %s is empty", s.SecretID)
	}

	var wrapped struct {
		PrivateKey string `json:"private_key"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && wrapped.PrivateKey != "" {
		return []byte(wrapped.PrivateKey), wrapped.Passphrase, nil
	}

	return []byte(raw), "", nil
}
