//go:build e2e

// Package e2e stands up the whole system and drives it the way S3 does.
//
// The stack is provisioned by the same Terraform module production uses, so a
// mistake in the IaC shows up here rather than at deploy time. The Lambdas are
// the real build artefacts, running in LocalStack's runtime.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tclocalstack "github.com/testcontainers/testcontainers-go/modules/localstack"
	"github.com/testcontainers/testcontainers-go/network"
)

// repoRoot is the module root, resolved from this file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return abs
}

// Stack is a running instance of the whole system.
type Stack struct {
	Endpoint string // reachable from the test process
	Internal string // reachable from sibling containers on the same network
	Network  string

	Bucket        string
	PackagesTable string
	StateTable    string

	PublishFunction string
	SigningSecretID string

	S3      *s3.Client
	Dynamo  *dynamodb.Client
	Lambda  *lambda.Client
	Secrets *secretsmanager.Client

	// SigningKey is the armored public half of the key the stack signs with.
	// Container tests import it to verify the repository for real.
	SigningKey []byte

	container *tclocalstack.LocalStackContainer
	tfDir     string
}

// StartStack brings up LocalStack, applies the Terraform, and installs a
// throwaway signing key.
//
// It is deliberately not shared between test functions: a test that asserts on
// published output needs to know nothing else is writing to the bucket.
func StartStack(t *testing.T) *Stack {
	t.Helper()
	requireTooling(t)

	ctx := context.Background()
	root := repoRoot(t)

	buildLambdas(t, root)

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("creating network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(context.Background()) })

	container, err := tclocalstack.Run(ctx, "localstack/localstack:3.8",
		testcontainers.WithEnv(map[string]string{
			"SERVICES": "s3,dynamodb,sqs,lambda,iam,sts,secretsmanager,events,logs,cloudwatch",
			// DynamoDB Local otherwise partitions tables by access key and
			// region, so a table the provider has just created can read back
			// as missing and the apply stalls waiting for it.
			"DYNAMODB_SHARE_DB":                  "1",
			"LAMBDA_RUNTIME_ENVIRONMENT_TIMEOUT": "120",
			// Lambda containers must join the same network as the test's
			// package-install containers so they can reach the bucket.
			"LAMBDA_DOCKER_NETWORK": net.Name,
			"DEBUG":                 "1",
		}),
		network.WithNetwork([]string{"localstack"}, net),
		testcontainers.WithHostConfigModifier(func(hc *dockercontainer.HostConfig) {
			// LocalStack launches Lambda containers via the host's Docker.
			hc.Binds = append(hc.Binds, "/var/run/docker.sock:/var/run/docker.sock")
		}),
	)
	if err != nil {
		t.Fatalf("starting localstack: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminating localstack: %v", err)
		}
	})

	host, err := container.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		t.Fatalf("resolving localstack endpoint: %v", err)
	}

	s := &Stack{
		Endpoint:  host,
		Internal:  "http://localstack:4566",
		Network:   net.Name,
		container: container,
		tfDir:     filepath.Join(root, "deploy", "terraform", "envs", "localstack"),
	}

	s.applyTerraform(t)
	s.connect(t)
	s.installSigningKey(t)

	return s
}

// requireTooling skips rather than fails when the machine cannot run the
// suite, so `go test -tags e2e ./...` is still useful on a laptop without
// Terraform installed.
func requireTooling(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not installed; skipping the end-to-end suite")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available; skipping the end-to-end suite")
	}
}

// buildLambdas produces the same zips a deploy would.
func buildLambdas(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("make", "build")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building lambdas: %v\n%s", err, out)
	}
}

func (s *Stack) applyTerraform(t *testing.T) {
	t.Helper()

	// A per-test state file, so a failed run leaves nothing behind for the
	// next one to trip over.
	stateFile := filepath.Join(t.TempDir(), "terraform.tfstate")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("terraform", args...)
		cmd.Dir = s.tfDir
		cmd.Env = append(os.Environ(),
			"TF_IN_AUTOMATION=1",
			"AWS_ACCESS_KEY_ID=test",
			"AWS_SECRET_ACCESS_KEY=test",
			"AWS_REGION=us-east-1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-backend=false", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color",
		"-state="+stateFile,
		"-var=endpoint="+s.Endpoint,
	)

	t.Cleanup(func() {
		cmd := exec.Command("terraform", "destroy", "-auto-approve", "-input=false", "-no-color",
			"-state="+stateFile, "-var=endpoint="+s.Endpoint)
		cmd.Dir = s.tfDir
		cmd.Env = append(os.Environ(),
			"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_REGION=us-east-1")
		// The container is torn down anyway; a destroy failure is noise.
		_ = cmd.Run()
	})

	out := s.terraformOutput(t, stateFile)
	s.Bucket = out["bucket"]
	s.PackagesTable = out["packages_table"]
	s.StateTable = out["state_table"]
	s.PublishFunction = out["publish_function"]
	s.SigningSecretID = out["signing_secret_id"]
}

func (s *Stack) terraformOutput(t *testing.T, stateFile string) map[string]string {
	t.Helper()

	cmd := exec.Command("terraform", "output", "-json", "-no-color", "-state="+stateFile)
	cmd.Dir = s.tfDir
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_REGION=us-east-1")

	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("terraform output: %v", err)
	}

	var decoded map[string]struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding terraform output: %v\n%s", err, raw)
	}

	out := make(map[string]string, len(decoded))
	for k, v := range decoded {
		out[k] = v.Value
	}
	return out
}

func (s *Stack) connect(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", "")),
		awsconfig.WithBaseEndpoint(s.Endpoint),
	)
	if err != nil {
		t.Fatalf("loading aws config: %v", err)
	}

	s.S3 = s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
	s.Dynamo = dynamodb.NewFromConfig(cfg)
	s.Lambda = lambda.NewFromConfig(cfg)
	s.Secrets = secretsmanager.NewFromConfig(cfg)
}

// installSigningKey stores the committed test key where the publisher expects
// to find it. The production key never leaves Secrets Manager, so the tests
// exercise the same code path with a key of their own.
//
// The key is a committed fixture generated by GnuPG rather than generated
// in-process, for two reasons. It matches production, where the signing key
// comes from a real OpenPGP toolchain. And it matters for what these tests
// prove: dnf verifies repomd.xml through librepo, which is stricter than Go's
// own verifier, so a key that has only ever round-tripped through Go would
// leave the strictest consumer untested.
func (s *Stack) installSigningKey(t *testing.T) {
	t.Helper()

	root := repoRoot(t)
	private, err := os.ReadFile(filepath.Join(root, "testdata", "signing", "test-key.private.asc"))
	if err != nil {
		t.Fatalf("reading test signing key: %v", err)
	}
	public, err := os.ReadFile(filepath.Join(root, "testdata", "signing", "test-key.public.asc"))
	if err != nil {
		t.Fatalf("reading test public key: %v", err)
	}
	s.SigningKey = public

	if _, err := s.Secrets.PutSecretValue(context.Background(), &secretsmanager.PutSecretValueInput{
		SecretId:     aws.String(s.SigningSecretID),
		SecretString: aws.String(string(private)),
	}); err != nil {
		t.Fatalf("storing signing key: %v", err)
	}
}

// PutConfig writes repos.yaml into the bucket. The Lambdas read it per
// invocation, so a test can change the distro matrix mid-run.
func (s *Stack) PutConfig(t *testing.T, yaml string) {
	t.Helper()
	s.PutObject(t, "repos.yaml", []byte(yaml), "application/yaml")
}

// PutObject uploads an object, which is what triggers the pipeline.
func (s *Stack) PutObject(t *testing.T, key string, body []byte, contentType string) {
	t.Helper()
	if _, err := s.S3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	}); err != nil {
		t.Fatalf("uploading %s: %v", key, err)
	}
}

// PutFixture uploads a committed fixture package.
func (s *Stack) PutFixture(t *testing.T, key, dir, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "packages", dir, name))
	if err != nil {
		t.Fatalf("reading fixture: %v (run ./testdata/generate.sh)", err)
	}
	s.PutObject(t, key, body, "application/octet-stream")
	return body
}

// DeleteObject removes an object, which is what triggers de-indexing.
func (s *Stack) DeleteObject(t *testing.T, key string) {
	t.Helper()
	if _, err := s.S3.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("deleting %s: %v", key, err)
	}
}

// GetObject reads a published artifact.
func (s *Stack) GetObject(ctx context.Context, key string) ([]byte, error) {
	out, err := s.S3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()

	return io.ReadAll(out.Body)
}

// WaitForObject polls until a key exists, and returns its body.
//
// The pipeline is asynchronous by design — an upload is coalesced into a
// publish that happens moments later — so tests wait for an outcome rather
// than assuming one.
func (s *Stack) WaitForObject(t *testing.T, key string, timeout time.Duration) []byte {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for {
		body, err := s.GetObject(ctx, key)
		if err == nil {
			return body
		}
		lastErr = err

		select {
		case <-ctx.Done():
			t.Fatalf("timed out after %s waiting for %s (last error: %v)\npublished so far:\n%s",
				timeout, key, lastErr, strings.Join(s.List(t, ""), "\n"))
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// WaitUntil polls until check passes.
func (s *Stack) WaitUntil(t *testing.T, what string, timeout time.Duration, check func() (bool, error)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := check()
		if ok {
			return
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (last error: %v)\npublished so far:\n%s",
		timeout, what, lastErr, strings.Join(s.List(t, ""), "\n"))
}

// List returns every key under a prefix, for assertions and failure messages.
func (s *Stack) List(t *testing.T, prefix string) []string {
	t.Helper()

	var (
		keys  []string
		token *string
	)
	for {
		page, err := s.S3.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return append(keys, fmt.Sprintf("(listing failed: %v)", err))
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(page.IsTruncated) {
			return keys
		}
		token = page.NextContinuationToken
	}
}

// Sweep invokes the publisher's scheduled path directly.
//
// EventBridge schedules are not reliably delivered by LocalStack's community
// edition, so the tests drive the sweep themselves rather than waiting on a
// timer that may never fire.
func (s *Stack) Sweep(t *testing.T) {
	t.Helper()

	out, err := s.Lambda.Invoke(context.Background(), &lambda.InvokeInput{
		FunctionName: aws.String(s.PublishFunction),
		Payload:      []byte(`{"source":"aws.events"}`),
	})
	if err != nil {
		t.Fatalf("invoking the publisher: %v", err)
	}
	if out.FunctionError != nil {
		t.Fatalf("publisher failed: %s\n%s", aws.ToString(out.FunctionError), out.Payload)
	}
}
