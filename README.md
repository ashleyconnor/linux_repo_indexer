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

Removing an entry stops it being published but **does not** delete the
published tree, so a typo in the config cannot destroy a live repository.
Tearing one down is deliberate and separate:

```bash
repoadmin retire --bucket B --scope rpm/RHEL/8/x86_64/stable --confirm
```

## Seeding an existing repository

Adopting the indexer for a repository that already exists reads the published
index, not the packages — the same reason the steady state never rescans.

```bash
seed apt --bucket B --source https://apt.releases.hashicorp.com \
         --codename noble --component main --arch amd64

seed rpm --bucket B --source https://rpm.releases.hashicorp.com \
         --tree RHEL/9/x86_64/stable
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
