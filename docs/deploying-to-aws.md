# Deploying to AWS and testing it

A runbook for standing up a real stack, seeding it from an existing public
repository, and proving that `apt` and `dnf` will install from what it
publishes.

Everything below assumes `us-east-1` and the bucket named in
`deploy/terraform/envs/aws/terraform.tfvars`. Substitute your own if you change
them.

## 0. Prerequisites

Working AWS credentials. The `default` profile currently holds static keys that
AWS rejects:

```bash
aws sts get-caller-identity --region us-east-1
```

That must print an account before anything else will work. The stack needs
permission to create S3, DynamoDB, SQS, Lambda, IAM, EventBridge, CloudWatch and
Secrets Manager resources.

Then build the Lambda bundles:

```bash
make build
```

## 1. Deploy

### 1a. Generate the signing key

Do this yourself — it is the repository's signing identity, and the private key
should not pass through a terminal whose scrollback is being read by anything
else.

The Lambda signs unattended, so the key must have no passphrase. Generate it
with a batch file:

```bash
cat > /tmp/indexer-key.batch <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 4096
Subkey-Type: RSA
Subkey-Length: 4096
Name-Real: Linux Repo Indexer
Name-Email: repo@example.com
Expire-Date: 0
%commit
EOF
gpg --batch --generate-key /tmp/indexer-key.batch
```

Export both halves. The private key goes to Secrets Manager; the public key gets
served from the bucket so clients can verify signatures:

```bash
KEYID=$(gpg --list-keys --with-colons repo@example.com | awk -F: '/^pub/{print $5; exit}')
gpg --armor --export-secret-keys "$KEYID" > /tmp/indexer-private.asc
gpg --armor --export            "$KEYID" > /tmp/indexer-public.asc
```

If you would rather keep a passphrase, store the secret as
`{"private_key": "...", "passphrase": "..."}` instead of the bare armored key —
the publisher accepts either.

### 1b. Apply

```bash
cd deploy/terraform/envs/aws
terraform init
terraform plan
```

Read the plan, then:

```bash
terraform apply
```

Capture the outputs — the rest of the runbook uses them:

```bash
cd deploy/terraform/envs/aws
export BUCKET=$(terraform output -raw bucket)
export BASE_URL=$(terraform output -raw base_url)
export PACKAGES_TABLE=$(terraform output -raw packages_table)
export STATE_TABLE=$(terraform output -raw state_table)
export PUBLISH_FN=$(terraform output -raw publish_function)
export SECRET_ID=$(terraform output -raw signing_secret_id)
cd -
```

### 1c. Load the key and the config

The secret exists but is empty until you write the key into it:

```bash
aws secretsmanager put-secret-value \
  --secret-id "$SECRET_ID" \
  --secret-string file:///tmp/indexer-private.asc \
  --region us-east-1
```

Publish the public key at the path the clients below expect:

```bash
aws s3 cp /tmp/indexer-public.asc "s3://$BUCKET/gpg" --content-type text/plain
```

Now write `repos.yaml`. **Uploading it is what starts the first publish** — the
S3 event marks every configured scope dirty, so do this after the key is in
place or the first build goes out unsigned.

```bash
cat > /tmp/repos.yaml <<EOF
version: 1
signing:
  secret_id: $SECRET_ID
apt:
  origin: Indexer Test
  label: Indexer Test
  acquire_by_hash: true
  compressions: [gz, xz]
  components: [main, test]
  architectures: [amd64, arm64]
  codenames: [noble]
rpm:
  trees:
    - distro: RHEL
      version: "9"
      architectures: [x86_64, aarch64]
      channels: [stable, test]
EOF

aws s3 cp /tmp/repos.yaml "s3://$BUCKET/repos.yaml" --content-type application/yaml
```

Confirm the publisher woke up and built empty indexes:

```bash
aws logs tail "/aws/lambda/$PUBLISH_FN" --since 5m --region us-east-1
aws s3 ls "s3://$BUCKET/dists/noble/"
```

You should see `Release`, `InRelease` and `Release.gpg`.

## 2. Seed from the live HashiCorp repositories

Seeding writes **records, not package files**. It reads the published index of
an existing repository and stores what it finds, which is the whole point: a
3426-package repository is adopted without downloading 3426 packages.

Check what it would do first — this needs no AWS at all:

```bash
go run ./cmd/seed apt \
  --source https://apt.releases.hashicorp.com \
  --codename noble --component main --arch arm64 --dry-run | head
```

Then seed for real. Four scopes, roughly 12,000 records in total:

```bash
export BUCKET PACKAGES_TABLE STATE_TABLE AWS_REGION=us-east-1

for arch in amd64 arm64; do
  go run ./cmd/seed apt \
    --source https://apt.releases.hashicorp.com \
    --codename noble --component main --arch "$arch"
done

for arch in x86_64 aarch64; do
  go run ./cmd/seed rpm \
    --source https://rpm.releases.hashicorp.com \
    --tree "RHEL/9/$arch/stable"
done
```

Seeding marks each scope dirty but does not enqueue a publish, so trigger one:

```bash
aws lambda invoke --function-name "$PUBLISH_FN" \
  --cli-binary-format raw-in-base64-out --payload '{}' \
  --region us-east-1 /dev/stdout
```

Verify the records landed and agree with the source:

```bash
go run ./cmd/seed verify --scope deb/main/arm64
go run ./cmd/seed diff  --scope deb/main/arm64 \
  --against https://apt.releases.hashicorp.com
```

**The index now lists thousands of packages whose `.deb` files are not in your
bucket.** `apt update` works; installing any of them 404s. Copy the few you
actually want installable — section 4 does this for `terraform`.

## 3. Exercise the pipeline with the test fixtures

These are real signed packages built by `testdata/generate.sh`, and uploading
them drives the full path: S3 event → ingest → DynamoDB → publish → index.

Fixtures are `arm64`/`aarch64` here so Docker runs natively on Apple silicon.
Swap to `amd64`/`x86_64` if you prefer, and add `--platform linux/amd64` to the
`docker run` commands in section 4.

```bash
aws s3 cp testdata/packages/deb/indexer-fixture_1.0.0-1_arm64.deb \
  "s3://$BUCKET/pool/arm64/main/indexer-fixture_1.0.0-1_arm64.deb"

aws s3 cp testdata/packages/rpm/indexer-fixture-1.0.0-1.aarch64.rpm \
  "s3://$BUCKET/RHEL/9/aarch64/stable/indexer-fixture-1.0.0-1.aarch64.rpm"
```

Watch it work:

```bash
aws logs tail "/aws/lambda/$(cd deploy/terraform/envs/aws && terraform output -raw ingest_function)" \
  --since 5m --follow --region us-east-1
```

Then confirm the package reached the index:

```bash
curl -fsS "$BASE_URL/dists/noble/main/binary-arm64/Packages" | grep -A2 indexer-fixture
curl -fsS "$BASE_URL/RHEL/9/aarch64/stable/repodata/repomd.xml" | head
```

Both dead-letter queues should be empty. Anything here is a package that failed
to index:

```bash
cd deploy/terraform/envs/aws
for q in ingest_dlq_url publish_dlq_url; do
  aws sqs get-queue-attributes --queue-url "$(terraform output -raw $q)" \
    --attribute-names ApproximateNumberOfMessages --region us-east-1
done
cd -
```

### Checking the fixes in this branch

Config fan-out — re-uploading `repos.yaml` should republish every scope with no
package upload involved:

```bash
aws s3 cp /tmp/repos.yaml "s3://$BUCKET/repos.yaml" --content-type application/yaml
aws logs tail "/aws/lambda/$PUBLISH_FN" --since 2m --region us-east-1 \
  | grep "repository config changed"
```

Lease contention — upload several packages at once and look for a requeue rather
than a silently dropped message:

```bash
aws s3 cp testdata/packages/deb/ "s3://$BUCKET/pool/arm64/main/" \
  --recursive --exclude '*' --include '*_arm64.deb'

aws logs tail "/aws/lambda/$PUBLISH_FN" --since 3m --region us-east-1 \
  | grep "requeuing"
```

## 4. Install from it

### Ubuntu, via apt

```bash
docker run --rm -e BASE_URL="$BASE_URL" ubuntu:24.04 bash -euxo pipefail -c '
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl >/dev/null

install -d -m 0755 /etc/apt/keyrings
curl -fsSL "$BASE_URL/gpg" -o /etc/apt/keyrings/indexer.asc

# signed-by pins the repo to our key, so this only succeeds if InRelease
# actually verifies against it.
echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/indexer.asc] $BASE_URL noble main" \
  > /etc/apt/sources.list.d/indexer.list

apt-get update
apt-get install -y indexer-fixture
indexer-fixture
'
```

### RHEL 9, via dnf

```bash
docker run --rm -e BASE_URL="$BASE_URL" \
  registry.access.redhat.com/ubi9/ubi bash -euxo pipefail -c '
cat > /etc/yum.repos.d/indexer.repo <<EOF
[indexer]
name=Indexer Test
baseurl=$BASE_URL/RHEL/9/aarch64/stable
enabled=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=$BASE_URL/gpg
EOF

rpm --import "$BASE_URL/gpg"

# Verify the pair by hand first: if this fails the signature genuinely does not
# match what is served, rather than dnf mishandling it.
curl -fsS "$BASE_URL/RHEL/9/aarch64/stable/repodata/repomd.xml" -o /tmp/repomd.xml
curl -fsS "$BASE_URL/RHEL/9/aarch64/stable/repodata/repomd.xml.asc" -o /tmp/repomd.xml.asc
curl -fsS "$BASE_URL/gpg" -o /tmp/key.asc
gpg --import /tmp/key.asc
gpg --verify /tmp/repomd.xml.asc /tmp/repomd.xml

# -y on makecache matters: without it dnf will not accept the key from gpgkey=,
# and reports the result as "Bad GPG signature" rather than a missing key.
dnf -y --disablerepo="*" --enablerepo=indexer makecache
dnf -y --disablerepo="*" --enablerepo=indexer install indexer-fixture
indexer-fixture
'
```

### Installing a real seeded package

Seeded records point at objects that are not in your bucket yet. Copy one
across at the exact key its record names, then install it:

```bash
TF_VER=$(curl -fsS "$BASE_URL/dists/noble/main/binary-arm64/Packages" \
  | awk '/^Package: terraform$/{f=1} f&&/^Version:/{print $2; exit}')

curl -fsSL -o /tmp/terraform.deb \
  "https://apt.releases.hashicorp.com/pool/arm64/main/terraform_${TF_VER}_arm64.deb"

aws s3 cp /tmp/terraform.deb \
  "s3://$BUCKET/pool/arm64/main/terraform_${TF_VER}_arm64.deb"
```

Uploading it re-ingests the package, so the record is replaced by one with a
recomputed SHA256 rather than the checksum inherited from the upstream index.
Then `apt-get install -y terraform` inside the Ubuntu container above.

## Notes from the first real deployment

**Upload `repos.yaml` immediately after the apply.** Both Lambdas load the
config before doing anything and fail the whole invocation without it. On a
fresh stack S3 sends one `s3:TestEvent` per notification rule the moment the
notification is created — five of them here — and every one failed its five
retries against the missing config and landed in the ingest dead-letter queue.
Harmless in itself, but a real package uploaded in that window would go the same
way, and the DLQ then reads as five failures that never happened.

**`scope` is a DynamoDB reserved word.** Ad-hoc queries against either table
need an expression attribute name:

```bash
aws dynamodb query --table-name linux-repo-indexer-packages \
  --key-condition-expression "#s = :s" \
  --expression-attribute-names '{"#s":"scope"}' \
  --expression-attribute-values '{":s":{"S":"deb/main/arm64"}}' \
  --select COUNT
```

`describe-table` reports `ItemCount`, but that figure is refreshed roughly every
six hours, so it reads 0 straight after a seed. Count with a query instead.

**Do not restrict apt to only this repository when installing a real package.**
Seeded packages carry real dependencies — `terraform` depends on `git` — so the
distribution's own sources have to stay enabled. Limiting `Dir::Etc::sourcelist`
to the indexer repo is useful for proving where a package resolves from, but the
install then fails on unmet dependencies.

## 5. Tear down

```bash
cd deploy/terraform/envs/aws
aws s3 rm "s3://$BUCKET" --recursive
terraform destroy
```

The bucket must be emptied first — Terraform will not delete a bucket with
objects in it. The signing secret is scheduled for deletion with a 30-day
recovery window rather than removed, so reusing the same `name_prefix` inside
that window needs:

```bash
aws secretsmanager delete-secret --secret-id "$SECRET_ID" \
  --force-delete-without-recovery --region us-east-1
```
