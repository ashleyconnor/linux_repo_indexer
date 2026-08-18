# linux-repo-indexer

Incrementally generates apt and yum repository metadata from packages in S3.

A package is parsed exactly once, when it is uploaded. Everything downstream —
the `Packages` file, `primary.xml`, the signed `Release` — is generated from
stored metadata, never by re-reading packages. That removes the full-repository
rescan that conventional tooling performs on every update: publishing becomes a
sequential pass over structured records rather than opening thousands of `.deb`
and `.rpm` files.

```
upload foo_1.2.3_amd64.deb
        │
        ▼
    S3 event ──► SQS ──► ingest Lambda
                           ├── parse metadata (one streaming pass, hashing as it reads)
                           ├── store record in DynamoDB
                           └── mark scope dirty, enqueue publish
                                    │
                                    ▼
                              publish Lambda
                                    ├── take the scope's lease
                                    ├── read every record in the scope
                                    ├── generate indexes, sign them
                                    └── sync to S3, prune superseded files
```

## What it produces

**apt** — one shared pool, published to every configured codename:

```
pool/<arch>/<component>/<name>_<version>_<arch>.deb
dists/<codename>/<component>/binary-<arch>/Packages{,.gz,.xz}
dists/<codename>/<component>/binary-<arch>/by-hash/{MD5Sum,SHA1,SHA256}/<hash>
dists/<codename>/{Release,Release.gpg,InRelease}
```

**yum** — a self-contained tree per distro, version, architecture and channel:

```
<Distro>/<version>/<arch>/<channel>/<name>-<ver>-<rel>.<arch>.rpm
<Distro>/<version>/<arch>/<channel>/repodata/{repomd.xml,repomd.xml.asc,<hash>-primary.xml.gz,…}
```

`primary.xml`, `filelists.xml` and `other.xml` are byte-identical to
`createrepo_c`'s output, which the tests assert strictly.

## Uploading packages

Publishing a package means putting it in the bucket. There is no API and no
build step to run: the key you write to is what decides where the package
appears, and the S3 event does the rest.

```bash
# Debian: the codename is NOT part of the path
aws s3 cp mypkg_1.0.0-1_arm64.deb "s3://$BUCKET/pool/arm64/main/mypkg_1.0.0-1_arm64.deb"

# RPM: the tree IS the path
aws s3 cp mypkg-1.0.0-1.aarch64.rpm "s3://$BUCKET/RHEL/9/aarch64/stable/mypkg-1.0.0-1.aarch64.rpm"
```

The two layouts differ in an important way. A `.deb` goes into one shared pool
keyed by architecture and component, and is then published into *every*
configured codename — upload once and the package appears in `noble` and
`trixie` alike, because the pool is what the codenames point at. A `.rpm` goes
into exactly one self-contained tree, and reaches only that distro, version,
architecture and channel.

| | Upload to | Reaches |
|---|---|---|
| `.deb` | `pool/<arch>/<component>/<file>.deb` | every codename |
| `.rpm` | `<Distro>/<version>/<arch>/<channel>/<file>.rpm` | that one tree |

The architecture appears twice in a Debian upload — once in the path and once
in the filename — and only the path is authoritative for routing. Note also
that the RPM name for 64-bit ARM is `aarch64` where Debian calls it `arm64`.

### One object per destination

The codename is the only dimension you get for free. Every other one — the
component, the architecture, and on the RPM side the distro, version and channel
— is a separate scope with its own records, so a package that belongs in two of
them has to exist at two keys.

A package for both `stable` and `test` on RHEL 9 and RHEL 10 is four objects:

```
RHEL/9/aarch64/stable/mypkg-1.0.0-1.aarch64.rpm
RHEL/9/aarch64/test/mypkg-1.0.0-1.aarch64.rpm
RHEL/10/aarch64/stable/mypkg-1.0.0-1.aarch64.rpm
RHEL/10/aarch64/test/mypkg-1.0.0-1.aarch64.rpm
```

The same holds for `main` and `test` components on the Debian side, though
`pool/arm64/main/` covers every codename at once.

You do not need to upload from your machine more than once. A server-side copy
never moves the bytes through your client, and still raises the
`s3:ObjectCreated:Copy` event that drives ingest:

```bash
aws s3 cp "s3://$BUCKET/RHEL/9/aarch64/stable/mypkg-1.0.0-1.aarch64.rpm" \
          "s3://$BUCKET/RHEL/10/aarch64/stable/mypkg-1.0.0-1.aarch64.rpm"
```

Each copy is parsed and stored independently, so the package is held once per
tree rather than shared. That is the cost of a self-contained yum tree: a client
pointed at one `baseurl` must find everything it needs underneath it.

Deleting an object works the same way in reverse: the package drops out of the
index on the next publish. Records are keyed by object, so the S3 key alone
identifies the row — the package does not need to be re-read to be removed.

### When a package does not appear

The path must have exactly four segments for a `.deb` and five for a `.rpm`.
Upstream Debian's `pool/main/t/terraform/` layout does not work here; this
scheme puts the architecture before the component and does not nest by
initial.

A key that does not classify — an unconfigured architecture, a component that is
not in `repos.yaml`, an extra directory level — is **logged and skipped, not
failed**. That is deliberate: one stray upload must not block the queue behind
it. The cost is that a misplaced package is silently absent rather than loudly
broken, so check the ingest log first:

```bash
aws logs tail "/aws/lambda/$NAME_PREFIX-ingest" --since 5m --follow
```

An `ingested` line means the record was stored. Nothing at all means the key was
skipped. An empty dead-letter queue alongside a missing package always points at
the path, not the pipeline.

One thing worth knowing before adding a new distro: S3 only sends events for the
prefixes in `package_prefixes` (`pool/`, `RHEL/`, `AmazonLinux/`, `fedora/` by
default), and they are case-sensitive. Adding a tree to `repos.yaml` whose path
starts with something else will index nothing at all until that prefix is added
to the module and Terraform is re-applied. The same applies if you override
`pool_prefix`, which is what makes the pool directory `pool` in the first
place — the Terraform prefix has to change with it.

## Adding and removing distributions

The distro matrix is data, not code. `repos.yaml` lives in the bucket and is
read on every invocation, so adding a codename or a yum tree takes effect
without a deploy:

```yaml
version: 1
signing:
  secret_id: linux-repo-indexer/gpg-signing-key
apt:
  origin: HashiCorp
  label: HashiCorp
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
```

Writing the file is itself the trigger: the upload fires an S3 event, and every
configured scope is marked dirty and republished. A new codename gets its
indexes without waiting for a package upload to prompt it, and a changed
`origin` or `compressions` is applied to output no upload would have touched.
Rebuilding everything is cheap because an unchanged index is never re-uploaded.

Removing an entry stops it being published but **does not** delete the
published tree, so a typo in the config cannot destroy a live repository.
Tearing one down is deliberate and separate:

```bash
repoadmin retire --bucket B --scope rpm/RHEL/8/x86_64/stable --confirm
```

### Carrying releases forward to a new distro version

A new yum tree starts empty. Adding `RHEL 10` alongside `RHEL 9` gives you valid
but empty `repodata`, because a tree is self-contained and nothing is shared
between versions. To carry the existing releases forward, copy the packages
across.

**On the apt side there is nothing to do.** Adding a codename needs no copying
at all: the pool is shared, so a new codename publishes the packages that are
already there. This section is only about yum trees.

Add the new version to `repos.yaml` first — the packages have to land somewhere
that publishes them — then let `repoadmin` do the copying. It reads the same
config, so the architectures and channels come from the file rather than from a
shell loop that can miss a pair:

```bash
repoadmin clone --bucket B --from RHEL/9 --to RHEL/10
```

That reports the plan and copies nothing:

```
RHEL/9/x86_64/stable    -> RHEL/10/x86_64/stable    3421 to copy
RHEL/9/x86_64/test      -> RHEL/10/x86_64/test         0 to copy (source is empty)
RHEL/9/aarch64/stable   -> RHEL/10/aarch64/stable   2472 to copy
RHEL/9/aarch64/test     -> RHEL/10/aarch64/test        0 to copy (all 12 already present)

5893 objects across 4 trees.

Nothing copied. Re-run with --confirm to proceed.
```

Add `--confirm` to perform it. There is no interactive prompt — the flag is the
only gate, so this runs unattended in CI — and re-running is safe: packages
already in the target are skipped, so an interrupted clone resumes rather than
starting again.

Only `.rpm` objects are copied. The old tree's `repodata` is left behind, which
is what you want: the publisher writes the new tree's metadata itself, and
copied metadata would be neither read nor valid.

The copies are server-side, so packages never travel through your machine, but
**every copied object is parsed as if newly uploaded**: ingest streams it,
hashes it, and writes a record. Carrying a few thousand packages forward is a
few thousand Lambda invocations and a full read of every package. It costs
little and needs no supervision, but it is not instant.

The command exits as soon as the copies are made, printing what to watch. Expect
the publish count to be far lower than the ingest count — the coalescing window
is doing its job. When the ingest queue is empty, confirm every record resolves
to an object that actually exists, because a record without an object is an index
entry that every client resolves to a 404:

```bash
seed verify --scope rpm/RHEL/10/aarch64/stable
```

That exits non-zero and lists the offenders if anything is missing, which is the
check worth running before you tell anyone the new version is live.

## Seeding an existing repository

Adopting the indexer for a repository that already exists reads the published
index, not the packages — the same reason the steady state never rescans.

```bash
seed apt --bucket B --source https://apt.releases.hashicorp.com \
         --codename noble --component main --arch amd64

seed rpm --bucket B --source https://rpm.releases.hashicorp.com \
         --tree RHEL/9/x86_64/stable
```

Add `--dry-run` to see what would be written. Against an http source that
needs no AWS access at all — no bucket, no tables, no credentials — so it is
safe to run against production before anything is provisioned:

```bash
seed apt --source https://apt.releases.hashicorp.com \
         --codename noble --component main --arch amd64 --dry-run
```

Before moving any traffic:

```bash
seed verify --bucket B --scope deb/main/amd64   # every record's object exists
seed diff   --bucket B --scope deb/main/amd64 \
            --against https://apt.releases.hashicorp.com/dists/noble/main/binary-amd64/Packages.gz
```

`seed diff` must report no differences. One known and deliberate difference in
the metadata itself: seeded RPM records keep the SHA1 `pkgid` from the existing
index, because recomputing SHA256 would mean downloading every package. Mixed
checksum types are legal — each `<checksum>` declares its own.

**Cutover must sign with the existing production key.** Signing with a new key
breaks `apt-get update` for every client that already trusts the old one.

## Development

```bash
make test     # unit and golden tests, no Docker needed
make build    # both Lambda packages, linux/arm64
make e2e      # end-to-end suites; needs Docker and Terraform
```

`make fixtures` rebuilds the test packages with real `dpkg-deb` and `rpmbuild`,
and `testdata/generate-golden.sh` regenerates the golden index files with real
`apt-ftparchive` and `createrepo_c`. Both are committed; regenerate only when
the fixture definitions change.

### Tests

- **Golden** — generated metadata compared against `apt-ftparchive` and
  `createrepo_c`. The RPM XML is compared byte for byte.
- **Pipeline E2E** — the same Terraform module production uses, applied to
  LocalStack, running the real Lambda artifacts: upload, wait, assert on what
  was published, then delete and assert it left the index.
- **Install** — a stock `ubuntu:24.04` and a stock UBI9 install a package from
  the generated repository with signature verification enabled. A negative
  control replaces `InRelease` with an unsigned copy and asserts apt refuses
  it, so a pass means verification actually happened.

`make e2e` sets `DOCKER_HOST` explicitly. On a machine that also has Podman
installed, testcontainers otherwise resolves `/var/run/docker.sock` to a
different daemon than the `docker` CLI uses.

## Design

`docs/superpowers/specs/2026-08-05-linux-repo-indexer-design.md` records the
architecture and, more usefully, the decisions that only became clear once the
formats were checked against the real tools.
