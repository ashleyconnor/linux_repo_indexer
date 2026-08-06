// Command seed populates the database from an already-published repository.
//
// This is the one-time cost of adopting the indexer for a repository that
// already exists. It reads the published index rather than the packages, so
// seeding thousands of packages costs a handful of requests instead of
// thousands of downloads — the same reason the steady-state design never
// rescans.
//
//	seed apt    --bucket b --source https://apt.releases.hashicorp.com \
//	            --codename noble --component main --arch amd64
//	seed rpm    --bucket b --source https://rpm.releases.hashicorp.com \
//	            --tree RHEL/9/x86_64/stable
//	seed verify --bucket b --scope deb/main/amd64
//	seed diff   --bucket b --scope deb/main/amd64 \
//	            --against https://apt.releases.hashicorp.com/dists/noble/main/binary-amd64/Packages.gz
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ashleyconnor/linux-repo-indexer/internal/awsx"
	"github.com/ashleyconnor/linux-repo-indexer/internal/pkgmeta"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
	"github.com/ashleyconnor/linux-repo-indexer/internal/seed"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()

	var err error
	switch os.Args[1] {
	case "apt":
		err = runAPT(ctx, log, os.Args[2:])
	case "rpm":
		err = runRPM(ctx, log, os.Args[2:])
	case "verify":
		err = runVerify(ctx, log, os.Args[2:])
	case "diff":
		err = runDiff(ctx, log, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "seed: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		log.Error("failed", "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `seed populates the database from an already-published repository.

  seed apt    --source URL --codename noble --component main --arch amd64
  seed rpm    --source URL --tree RHEL/9/x86_64/stable
  seed verify --scope SCOPE
  seed diff   --scope SCOPE --against URL

--source is an http(s) base URL, or "s3" to read the index from the bucket.

--bucket, --packages-table and --state-table (or BUCKET, PACKAGES_TABLE and
STATE_TABLE) are required for anything that touches AWS. A --dry-run against an
http source needs none of them: it reads the published index, prints what it
found, and writes nothing.
`)
}

// common flags shared by every subcommand.
type common struct {
	bucket string
	table  string
	state  string
	dryRun bool
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.bucket, "bucket", os.Getenv("BUCKET"),
		"repository bucket; needed to read an index with --source s3, to check objects exist, "+
			"and to hold records too large for a DynamoDB item")
	fs.StringVar(&c.table, "packages-table", os.Getenv("PACKAGES_TABLE"), "packages table")
	fs.StringVar(&c.state, "state-table", os.Getenv("STATE_TABLE"), "publish state table")
	fs.BoolVar(&c.dryRun, "dry-run", false, "report what would be written without writing it")
}

// connect opens the AWS clients, or returns nil when the command will not
// touch AWS at all.
//
// A dry run against an http source reads a published index and prints what it
// found: it writes nothing and reads nothing from the bucket or the tables.
// Demanding credentials and resource names for that is pure friction, and
// invites people to invent placeholder values that then look real in a shell
// history.
//
// Everything else needs the bucket, including the seeding runs that appear to
// touch only DynamoDB: a package with a very large file list does not fit in
// an item and spills to S3 instead.
func (c *common) connect(ctx context.Context, source string) (*awsx.Clients, error) {
	if c.dryRun && !readsFromBucket(source) {
		return nil, nil
	}

	var missing []string
	if c.bucket == "" {
		missing = append(missing, "--bucket")
	}
	if c.table == "" {
		missing = append(missing, "--packages-table")
	}
	if c.state == "" {
		missing = append(missing, "--state-table")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("seed: %s required", strings.Join(missing, ", "))
	}

	return awsx.Connect(ctx, awsx.Env{
		Bucket:        c.bucket,
		PackagesTable: c.table,
		StateTable:    c.state,
		ConfigKey:     "repos.yaml",
	})
}

// readsFromBucket reports whether the index itself comes from S3 rather than
// over http.
func readsFromBucket(source string) bool { return source == "s3" }

func runAPT(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("apt", flag.ExitOnError)
	var c common
	c.bind(fs)

	source := fs.String("source", "", "base URL of the published repository, or \"s3\"")
	codename := fs.String("codename", "", "codename whose index to read; any one will do, since the pool is shared")
	component := fs.String("component", "main", "component to seed")
	arch := fs.String("arch", "", "architecture to seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *arch == "" || *codename == "" || *source == "" {
		return errors.New("seed apt: --source, --codename and --arch are required")
	}

	clients, err := c.connect(ctx, *source)
	if err != nil {
		return err
	}

	scope := repoconfig.DebPoolScope(*component, *arch)

	// The pool is shared across codenames, so one codename's index describes
	// every package in this component and architecture.
	rel := path.Join("dists", *codename, *component, "binary-"+*arch, "Packages.gz")
	body, err := fetch(ctx, clients, *source, rel)
	if err != nil {
		return err
	}

	pkgs, err := seed.ParsePackages(bytes.NewReader(body), time.Now().UTC())
	if err != nil {
		return err
	}
	log.Info("parsed index", "scope", scope.String(), "packages", len(pkgs), "source", rel)

	return write(ctx, log, clients, &c, scope, pkgs)
}

func runRPM(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("rpm", flag.ExitOnError)
	var c common
	c.bind(fs)

	source := fs.String("source", "", "base URL of the published repository, or \"s3\"")
	tree := fs.String("tree", "", "tree to seed, as <distro>/<version>/<arch>/<channel>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tree == "" || *source == "" {
		return errors.New("seed rpm: --source and --tree are required")
	}

	parts := strings.Split(strings.Trim(*tree, "/"), "/")
	if len(parts) != 4 {
		return fmt.Errorf("seed rpm: --tree %q is not <distro>/<version>/<arch>/<channel>", *tree)
	}
	scope := repoconfig.RPMTreeScope(parts[0], parts[1], parts[2], parts[3])

	clients, err := c.connect(ctx, *source)
	if err != nil {
		return err
	}

	repomd, err := fetch(ctx, clients, *source, path.Join(*tree, "repodata", "repomd.xml"))
	if err != nil {
		return err
	}
	locations, err := repomdLocations(repomd)
	if err != nil {
		return err
	}

	// filelists carries the complete file list and other the changelogs;
	// primary alone would produce a repository that cannot resolve file
	// dependencies.
	readers := map[string]io.Reader{}
	for _, kind := range []string{"primary", "filelists", "other"} {
		href, ok := locations[kind]
		if !ok {
			if kind == "primary" {
				return fmt.Errorf("seed rpm: repomd.xml has no primary entry")
			}
			log.Warn("repository publishes no metadata of this kind", "kind", kind)
			continue
		}
		body, err := fetch(ctx, clients, *source, path.Join(*tree, href))
		if err != nil {
			return err
		}
		readers[kind] = bytes.NewReader(body)
	}

	pkgs, err := seed.ParseRepodata(readers["primary"], readers["filelists"], readers["other"], *tree, time.Now().UTC())
	if err != nil {
		return err
	}
	log.Info("parsed repodata", "scope", scope.String(), "packages", len(pkgs))

	// The live index identifies packages by SHA1, which is kept rather than
	// recomputed. Say so, because it is a visible difference in the output.
	var sha1Only int
	for i := range pkgs {
		if pkgs[i].SHA256 == "" {
			sha1Only++
		}
	}
	if sha1Only > 0 {
		log.Info("packages carry a SHA1 checksum from the existing index; recomputing SHA256 would mean downloading each package",
			"packages", sha1Only)
	}

	return write(ctx, log, clients, &c, scope, pkgs)
}

// write stores the records and marks the scope for republication.
func write(ctx context.Context, log *slog.Logger, clients *awsx.Clients, c *common, scope repoconfig.Scope, pkgs []pkgmeta.Package) error {
	if c.dryRun {
		log.Info("dry run: nothing written", "scope", scope.String(), "packages", len(pkgs))
		for i := range pkgs {
			fmt.Printf("%s\t%s\t%s\n", scope, pkgs[i].String(), pkgs[i].Filename)
		}
		return nil
	}

	if clients == nil {
		// Unreachable: connect only declines for a dry run, which returned
		// above. Checked anyway so a future caller gets a diagnosis rather
		// than a nil dereference.
		return errors.New("seed: no AWS clients; --bucket, --packages-table and --state-table are required to write")
	}

	for i := range pkgs {
		if err := clients.Packages.Put(ctx, scope, &pkgs[i]); err != nil {
			return err
		}
		if (i+1)%500 == 0 {
			log.Info("progress", "scope", scope.String(), "written", i+1, "of", len(pkgs))
		}
	}

	// Seeding is only half the job: the scope has to be republished for the
	// records to become the index clients actually see.
	if _, err := clients.State.MarkDirty(ctx, scope); err != nil {
		return err
	}
	log.Info("seeded", "scope", scope.String(), "packages", len(pkgs))
	return nil
}

func runVerify(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var c common
	c.bind(fs)
	scopeArg := fs.String("scope", "", "scope to verify")
	if err := fs.Parse(args); err != nil {
		return err
	}

	scope, err := repoconfig.ParseScope(*scopeArg)
	if err != nil {
		return err
	}
	clients, err := c.connect(ctx, "s3")
	if err != nil {
		return err
	}

	pkgs, err := clients.Packages.List(ctx, scope)
	if err != nil {
		return err
	}

	// A record whose object is missing would put an entry in the index that
	// every client resolves to a 404.
	var missing int
	for i := range pkgs {
		_, err := clients.S3.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(c.bucket),
			Key:    aws.String(pkgs[i].S3Key),
		})
		if err != nil {
			missing++
			fmt.Printf("MISSING\t%s\t%s\n", pkgs[i].String(), pkgs[i].S3Key)
		}
	}

	log.Info("verified", "scope", scope.String(), "packages", len(pkgs), "missing", missing)
	if missing > 0 {
		return fmt.Errorf("seed verify: %d of %d objects are missing", missing, len(pkgs))
	}
	return nil
}

// runDiff compares what we would publish against what is published today.
//
// This is the cutover gate: it must report no differences before any traffic
// is moved.
func runDiff(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	var c common
	c.bind(fs)
	scopeArg := fs.String("scope", "", "scope to compare")
	against := fs.String("against", "", "URL of the currently published index")
	if err := fs.Parse(args); err != nil {
		return err
	}

	scope, err := repoconfig.ParseScope(*scopeArg)
	if err != nil {
		return err
	}
	if scope.Kind != repoconfig.KindDebPool {
		return errors.New("seed diff: only deb pool scopes are supported")
	}
	clients, err := c.connect(ctx, "s3")
	if err != nil {
		return err
	}

	ours, err := clients.Packages.List(ctx, scope)
	if err != nil {
		return err
	}

	body, err := fetchURL(ctx, *against)
	if err != nil {
		return err
	}
	theirs, err := seed.ParsePackages(bytes.NewReader(body), time.Now().UTC())
	if err != nil {
		return err
	}

	// Compare on identity and checksum: a difference in either is a package a
	// client would resolve differently.
	index := func(pkgs []pkgmeta.Package) map[string]string {
		m := make(map[string]string, len(pkgs))
		for i := range pkgs {
			m[pkgs[i].Key()] = pkgs[i].SHA256
		}
		return m
	}
	a, b := index(ours), index(theirs)

	var differences int
	for key, sum := range b {
		switch got, ok := a[key]; {
		case !ok:
			differences++
			fmt.Printf("ONLY_PUBLISHED\t%s\n", key)
		case got != sum:
			differences++
			fmt.Printf("CHECKSUM\t%s\tours=%s published=%s\n", key, got, sum)
		}
	}
	for key := range a {
		if _, ok := b[key]; !ok {
			differences++
			fmt.Printf("ONLY_SEEDED\t%s\n", key)
		}
	}

	log.Info("compared", "scope", scope.String(), "seeded", len(a), "published", len(b), "differences", differences)
	if differences > 0 {
		return fmt.Errorf("seed diff: %d differences; do not cut over", differences)
	}
	return nil
}

// fetch reads an index file from an http(s) base URL or from the bucket.
func fetch(ctx context.Context, clients *awsx.Clients, source, rel string) ([]byte, error) {
	if readsFromBucket(source) {
		if clients == nil {
			return nil, errors.New("seed: --source s3 needs --bucket")
		}
		return maybeGunzip(clients.S3.GetBlob(ctx, rel))
	}
	return fetchURL(ctx, strings.TrimSuffix(source, "/")+"/"+rel)
}

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: fetching %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("seed: reading %s: %w", url, err)
	}
	return maybeGunzip(body, nil)
}

// maybeGunzip transparently decompresses, since index files are published both
// compressed and plain and the caller should not have to care.
func maybeGunzip(body []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("seed: decompressing: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// repomdLocations maps each metadata kind to its tree-relative href.
func repomdLocations(body []byte) (map[string]string, error) {
	var doc struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("seed: parsing repomd.xml: %w", err)
	}

	out := make(map[string]string, len(doc.Data))
	for _, d := range doc.Data {
		out[d.Type] = d.Location.Href
	}
	return out, nil
}
