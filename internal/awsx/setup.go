package awsx

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
	"github.com/ashleyconnor/linux-repo-indexer/internal/store"
)

// Env is the configuration both Lambdas take from their environment.
type Env struct {
	Bucket          string
	ConfigKey       string
	PackagesTable   string
	StateTable      string
	PublishQueueURL string
	PublishDelay    time.Duration
	LeaseTTL        time.Duration
}

// LoadEnv reads the environment, failing loudly on anything missing. A Lambda
// that starts with a blank table name would otherwise fail one request at a
// time rather than once at deploy.
func LoadEnv() (Env, error) {
	e := Env{
		Bucket:          os.Getenv("BUCKET"),
		ConfigKey:       envOr("CONFIG_KEY", "repos.yaml"),
		PackagesTable:   os.Getenv("PACKAGES_TABLE"),
		StateTable:      os.Getenv("STATE_TABLE"),
		PublishQueueURL: os.Getenv("PUBLISH_QUEUE_URL"),
		PublishDelay:    envDuration("PUBLISH_DELAY_SECONDS", 30*time.Second),
		LeaseTTL:        envDuration("LEASE_TTL_SECONDS", 15*time.Minute),
	}

	for name, value := range map[string]string{
		"BUCKET":         e.Bucket,
		"PACKAGES_TABLE": e.PackagesTable,
		"STATE_TABLE":    e.StateTable,
	} {
		if value == "" {
			return Env{}, fmt.Errorf("awsx: %s is not set", name)
		}
	}
	return e, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// Clients holds the AWS clients and the stores built over them.
type Clients struct {
	Env      Env
	S3       *S3
	Packages *store.PackageStore
	State    *store.StateStore
	Queue    *SQSQueue
	Secrets  *secretsmanager.Client
}

// Connect builds the clients from the ambient AWS configuration.
//
// AWS_ENDPOINT_URL is honoured by the SDK itself, which is what lets the same
// binaries run against LocalStack in the end-to-end tests and against AWS in
// production without a build tag or a code path of their own.
func Connect(ctx context.Context, env Env) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("awsx: loading aws config: %w", err)
	}

	// LocalStack serves S3 on a single host, so virtual-host addressing does
	// not resolve.
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = os.Getenv("AWS_ENDPOINT_URL") != ""
	})

	bucket := &S3{Client: s3Client, Bucket: env.Bucket}
	dyn := dynamodb.NewFromConfig(cfg)

	c := &Clients{
		Env:      env,
		S3:       bucket,
		Packages: store.NewPackageStore(dyn, env.PackagesTable, bucket),
		State:    store.NewStateStore(dyn, env.StateTable),
		Secrets:  secretsmanager.NewFromConfig(cfg),
	}
	if env.PublishQueueURL != "" {
		c.Queue = &SQSQueue{Client: sqs.NewFromConfig(cfg), QueueURL: env.PublishQueueURL}
	}
	return c, nil
}

// LoadRepoConfig reads and validates repos.yaml from the bucket.
//
// It is read per invocation rather than cached: the whole point of keeping the
// distro matrix in the bucket is that adding a codename takes effect without a
// deploy, and a container that cached it could serve a stale matrix for hours.
func (c *Clients) LoadRepoConfig(ctx context.Context) (*repoconfig.Config, error) {
	raw, err := c.S3.GetConfig(ctx, c.Env.ConfigKey)
	if err != nil {
		return nil, fmt.Errorf("awsx: reading s3://%s/%s: %w", c.Env.Bucket, c.Env.ConfigKey, err)
	}
	return repoconfig.Parse(raw)
}

// Logger returns a JSON logger, which is what CloudWatch Logs Insights can
// query without a parser.
func Logger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
