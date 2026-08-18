# Deployment identity

`deployer-policy.json` is the permission set Terraform needs to create, update
and destroy this stack, plus the runtime calls the
[deployment runbook](../../docs/deploying-to-aws.md) makes: uploading packages,
seeding DynamoDB, invoking the publisher and reading the dead-letter queues.

It is derived from the resources the module actually declares — four SQS queues,
two DynamoDB tables, two Lambdas and their event source mappings, one IAM role,
one EventBridge rule, one bucket, one secret and two alarms — rather than from a
service-wildcard shortcut. Every statement is scoped to the ARNs that
`name_prefix` and `bucket_name` produce, so it cannot touch anything else in the
account.

## This is a privileged identity

It can create an IAM role and pass it to Lambda, which is the classic
privilege-escalation path. Two things narrow it:

- Every `iam:*` action is scoped to the single role ARN `NAME_PREFIX-lambda`.
- `iam:AttachRolePolicy` carries a condition allowing exactly one managed
  policy, `AWSLambdaBasicExecutionRole`. Without it, an identity that can attach
  arbitrary policies to a role it can pass is equivalent to an administrator.

Use it for a dedicated deployment principal. Do not reuse it as the application
runtime role — the Lambdas get their own, created by the module.

## What it is pinned to

Both documents are filled in for the live target, so they can be applied as-is:

| | |
|---|---|
| Account | `744513097645` |
| Region | `us-east-1` |
| `name_prefix` | `linux-repo-indexer` |
| `bucket_name` | `hcp-linux-repo-indexer` |

The one remaining placeholder is `DEPLOYER_USER` in the trust policy, which is
whichever IAM user assumes the role. If you retarget any of the above, the ARNs
are literal — search and replace, then re-check the size against the limits
below.

## Creating the role

```bash
sed -e "s/DEPLOYER_USER/your-iam-username/" \
    deploy/iam/deployer-trust-policy.json > /tmp/trust.json

aws iam create-role \
  --role-name linux-repo-indexer-deployer \
  --assume-role-policy-document file:///tmp/trust.json

aws iam put-role-policy \
  --role-name linux-repo-indexer-deployer \
  --policy-name terraform-deploy \
  --policy-document file://deploy/iam/deployer-policy.json
```

## Size

6,243 characters once IAM strips whitespace.

- Inline role policy (10,240): fits, 3,997 spare.
- Managed policy (6,144): **does not fit**, over by 99.

Attach it inline, as the `put-role-policy` above does. Splitting it across two
managed policies would work but buys nothing here.

## Tagging event source mappings

`default_tags` on the provider propagates to every taggable resource, including
Lambda event source mappings. Their ARNs contain a UUID that does not exist
until creation, so `lambda:TagResource` cannot be scoped to a named mapping the
way the function statement is — hence the separate `TagEventSourceMappings`
statement over `event-source-mapping:*`.

This was missed on the first apply and surfaced as an `AccessDenied` after 21 of
23 resources had been created. Terraform is idempotent here: fix the policy and
re-apply, and it creates only what is missing.

The trust policy requires MFA. Drop the condition if the principal assuming it
cannot present one.

Then point Terraform at it by adding a profile to `~/.aws/config`:

```ini
[profile indexer-deploy]
role_arn       = arn:aws:iam::ACCOUNT_ID:role/linux-repo-indexer-deployer
source_profile = default
region         = us-east-1
mfa_serial     = arn:aws:iam::ACCOUNT_ID:mfa/your-device
```

and setting `profile = "indexer-deploy"` in `terraform.tfvars`.

## Attaching to a user instead

If you would rather not use a role, the same document works as an inline user
policy — skip the trust policy entirely:

```bash
aws iam put-user-policy \
  --user-name your-iam-username \
  --policy-name linux-repo-indexer-deploy \
  --policy-document file:///tmp/deployer-policy.json
```

## The two statements that are not resource-scoped

Both are labelled as such in the document, because AWS does not support
resource-level permissions for them:

- `lambda:*EventSourceMapping` — mappings have no ARN to scope against at
  creation time. An `ArnLikeIfExists` condition on `lambda:FunctionArn` keeps it
  to this stack's functions where the key is present. The operator must be
  `ArnLike` rather than `StringLike`: `lambda:FunctionArn` is an ARN-typed key,
  and IAM Access Analyzer flags a string operator on one as a security finding,
  because string matching does not apply ARN segment semantics.
- `cloudwatch:DescribeAlarms` — read-only, and the alarms are only created when
  `alarm_actions` is set.

## Verifying before you apply

`terraform plan` exercises read paths only, so a successful plan does not prove
the create permissions are complete. To test the whole set without guessing,
run the apply and let it fail loudly — an `AccessDenied` names the exact missing
action. Alternatively dry-run individual calls with the IAM policy simulator:

```bash
aws iam simulate-principal-policy \
  --policy-source-arn arn:aws:iam::ACCOUNT_ID:role/linux-repo-indexer-deployer \
  --action-names lambda:CreateFunction iam:PassRole s3:CreateBucket \
  --resource-arns "arn:aws:lambda:us-east-1:ACCOUNT_ID:function:linux-repo-indexer-ingest"
```
