# repoadmin clone

Copies every package from one yum distro version into another, so a new version
starts with the releases the old one already carries.

## Why

A yum tree is self-contained: nothing is shared between `RHEL/9` and `RHEL/10`,
so adding a version to `repos.yaml` produces valid but empty `repodata`.
Carrying the existing releases forward means copying the package objects into
each new tree, and the S3 event those copies raise does the rest.

The apt side has no equivalent problem. A new codename publishes the packages
already in the shared pool, so nothing is copied and this command does not apply.

Today the operation is a shell loop over `aws s3 cp --recursive`. That works,
but the arch and channel combinations are typed by hand, and a missed pair is a
tree that silently publishes nothing. The matrix is already declared in
`repos.yaml`; the command reads it instead.

## Interface

```
repoadmin clone --from RHEL/9 --to RHEL/10 [--confirm]
```

`--from` and `--to` are `<distro>/<version>` pairs, not full scopes: the point is
to fan out across the architectures and channels rather than name one tree.

Dry run by default, as `retire` is. Without `--confirm` it prints the plan and
changes nothing. With it, it copies.

`--confirm` is the only gate: there is no interactive prompt, unlike `retire`.
Carrying a version forward belongs in CI, where there is nobody to answer one,
and the operation only adds objects — copying a package that is already there is
a no-op, so a mistaken run costs nothing that a mistaken `retire` would.

```
RHEL/9/x86_64/stable   -> RHEL/10/x86_64/stable    3421 to copy
RHEL/9/x86_64/test     -> RHEL/10/x86_64/test         0 to copy (source is empty)
RHEL/9/aarch64/stable  -> RHEL/10/aarch64/stable   2472 to copy
RHEL/9/aarch64/test    -> RHEL/10/aarch64/test         0 to copy (already present)

5893 objects across 4 trees.
Nothing copied. Re-run with --confirm to proceed.
```

## Planning

The target's configuration drives the matrix. For each architecture and channel
the `--to` tree declares in `repos.yaml`:

1. List `<from-distro>/<from-version>/<arch>/<channel>/` in S3.
2. Keep keys ending in `.rpm`.
3. Subtract the keys already present under the target prefix.

Deriving from the target rather than the source has two consequences worth
stating. An architecture the new version adds and the old one never had yields
an empty source listing and is reported as such, rather than being an error. And
the source version does not need to be in `repos.yaml` at all, so a version
already removed from the config can still be cloned from while its objects
remain.

Filtering on the `.rpm` suffix replaces the `--exclude "repodata/*"` that the
shell equivalent needs. It is stricter: metadata, checksum files and anything
else that has accumulated in the source tree cannot be carried across.

Subtracting existing keys makes the command idempotent and resumable. Re-running
after an interruption copies only what is missing, which matters because every
copied object is re-parsed by ingest — a needless second pass over 3000 packages
is real time and real money.

## Execution

Server-side `CopyObject` per key. The bytes never travel through the client, and
each copy raises `s3:ObjectCreated:Copy`, which ingest handles exactly as it
handles an upload: parse, hash, store a record against the target scope.

The command does not wait. Ingest and publish catch up asynchronously, and a
clone of a few thousand packages takes minutes to drain. Blocking that long is
awkward interactively and worse in CI, so the command exits printing the two
commands that answer "is it done": the ingest queue depth, and `seed verify`
against each target scope.

Nothing is written to DynamoDB directly. Copying the records would be pointless:
the S3 event re-parses the package regardless, so the record would be written
twice and the second write would win.

## Refusals

- A `--to` tree that is not in `repos.yaml` is an error. Copying into a tree
  nothing publishes is the one outcome with no use, and it fails at the plan
  stage rather than after 3000 copies.
- An argument naming a deb scope or codename is an error explaining that the
  pool is shared and a new codename needs no copying. That misconception is the
  most likely reason someone reaches for this command wrongly.
- Malformed `--from` or `--to` — anything that is not `<distro>/<version>` — is
  an error naming the expected shape.

## Structure

Logic lives in a new `internal/clone` package so it can be tested without AWS,
with `cmd/repoadmin` staying a thin front end. That matches the rest of the
repository, where `cmd` parses flags and `internal` does the work.

```go
// A Version names one distro version, the <distro>/<version> half of a tree
// path. Parse rejects anything that is not exactly two segments.
type Version struct {
    Distro  string
    Version string
}

func ParseVersion(s string) (Version, error)

// Trees expands the target version against the config, returning the source
// and target prefixes to compare. Listing is the caller's job.
func Trees(cfg *repoconfig.Config, from, to Version) ([]TreePair, error)

type TreePair struct {
    From, To       repoconfig.Scope
    FromDir, ToDir string
}

// Plan decides what to copy for one tree, given what exists on both sides.
func Plan(pair TreePair, source, target []string) TreeCopy

type TreeCopy struct {
    TreePair
    Copies []Copy // source key -> target key
    Note   string // why there is nothing to do, for the report
}

type Copy struct{ From, To string }

// Copier is the narrow S3 surface Execute needs.
type Copier interface {
    List(ctx context.Context, prefix string) ([]string, error)
    Copy(ctx context.Context, srcKey, dstKey string) error
}
```

Splitting `Trees` from `Plan` keeps both pure and separately testable: `Trees`
covers matrix expansion and the refusals, `Plan` covers `.rpm` filtering and
subtraction of what is already there. Neither touches AWS, so both are exercised
by table tests with no fakes at all. `Execute` is the only part needing a fake.

The config fields this relies on are already exported — `cfg.RPM.Trees` carries
`Distro`, `Version`, `Architectures` and `Channels`, and `repoconfig` provides
`RPMTreeScope` and `TreeDir` to turn a tree into a scope and a prefix.

## Testing

Against `Trees`:

- expands to every architecture and channel the target declares
- errors when the target version is not in `repos.yaml`
- errors on a deb scope or codename, naming the shared pool as the reason
- errors on a malformed version pair

Against `Plan`:

- omits keys already present in the target
- omits everything that is not a `.rpm`
- reports an empty source tree distinctly from a fully populated target
- maps each source key to the same filename under the target prefix

`Execute` gets a fake `Copier` asserting that each planned key is copied once and
that a failure stops rather than continuing silently.

## Out of scope

`--wait`, `--arch` and `--channel` filters, cross-channel promotion
(`stable` → `test`), and copying DynamoDB records. Each is a plausible next
feature and none is needed for the workflow this exists to serve.
