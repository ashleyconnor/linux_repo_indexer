// Command repoadmin performs the destructive operations the service will not
// do on its own.
//
// Removing a distribution from repos.yaml stops it being published but leaves
// its tree in place, so a typo in the config cannot delete a live repository.
// Actually tearing one down is this command's job, and it asks first.
//
//	repoadmin retire --bucket b --scope rpm/RHEL/8/x86_64/stable --confirm
//	repoadmin status --bucket b
//	repoadmin clone --bucket b --from RHEL/9 --to RHEL/10 --confirm
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ashleyconnor/linux-repo-indexer/internal/awsx"
	"github.com/ashleyconnor/linux-repo-indexer/internal/clone"
	"github.com/ashleyconnor/linux-repo-indexer/internal/repoconfig"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "retire":
		err = runRetire(ctx, os.Args[2:])
	case "status":
		err = runStatus(ctx, os.Args[2:])
	case "clone":
		err = runClone(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "repoadmin: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "repoadmin: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `repoadmin performs the destructive operations the service will not.

  repoadmin retire --bucket B --scope SCOPE [--confirm]
  repoadmin status --bucket B
  repoadmin clone  --bucket B --from RHEL/9 --to RHEL/10 [--confirm]

retire deletes a scope's published index and its stored records. It does not
delete package objects: those are the only copy of themselves.

clone carries a yum version's packages forward into a new one, which is what a
self-contained tree needs and a shared deb pool does not. It reports the plan
and copies nothing until --confirm.
`)
}

type flags struct {
	bucket string
	table  string
	state  string
}

func (f *flags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.bucket, "bucket", os.Getenv("BUCKET"), "repository bucket")
	fs.StringVar(&f.table, "packages-table", os.Getenv("PACKAGES_TABLE"), "packages table")
	fs.StringVar(&f.state, "state-table", os.Getenv("STATE_TABLE"), "publish state table")
}

func (f *flags) connect(ctx context.Context) (*awsx.Clients, error) {
	if f.bucket == "" || f.table == "" || f.state == "" {
		return nil, errors.New("--bucket, --packages-table and --state-table are all required")
	}
	return awsx.Connect(ctx, awsx.Env{
		Bucket:        f.bucket,
		PackagesTable: f.table,
		StateTable:    f.state,
		ConfigKey:     "repos.yaml",
	})
}

func runRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retire", flag.ExitOnError)
	var f flags
	f.bind(fs)
	scopeArg := fs.String("scope", "", "scope to retire")
	confirm := fs.Bool("confirm", false, "actually delete, rather than reporting what would be deleted")
	if err := fs.Parse(args); err != nil {
		return err
	}

	scope, err := repoconfig.ParseScope(*scopeArg)
	if err != nil {
		return err
	}
	clients, err := f.connect(ctx)
	if err != nil {
		return err
	}

	cfg, err := clients.LoadRepoConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.Knows(scope) {
		return fmt.Errorf("%s is still in repos.yaml; remove it there first, so nothing republishes it after this runs", scope)
	}

	dir, err := publishedDir(cfg, scope)
	if err != nil {
		return err
	}

	keys, err := listUnder(ctx, clients, f.bucket, dir)
	if err != nil {
		return err
	}
	pkgs, err := clients.Packages.List(ctx, scope)
	if err != nil {
		return err
	}

	fmt.Printf("scope:            %s\n", scope)
	fmt.Printf("index objects:    %d under %s/\n", len(keys), dir)
	fmt.Printf("package records:  %d\n", len(pkgs))
	fmt.Printf("package objects:  left in place; they are the only copy of themselves\n\n")

	if !*confirm {
		fmt.Println("Nothing deleted. Re-run with --confirm to proceed.")
		return nil
	}
	if !askForConfirmation(scope.String()) {
		fmt.Println("Aborted.")
		return nil
	}

	for chunk := range chunks(keys, 1000) {
		objects := make([]s3types.ObjectIdentifier, 0, len(chunk))
		for _, k := range chunk {
			objects = append(objects, s3types.ObjectIdentifier{Key: aws.String(k)})
		}
		if _, err := clients.S3.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(f.bucket),
			Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("deleting index objects: %w", err)
		}
	}

	for i := range pkgs {
		if err := clients.Packages.Delete(ctx, scope, pkgs[i].Filename); err != nil {
			return fmt.Errorf("deleting record for %s: %w", pkgs[i].String(), err)
		}
	}

	fmt.Printf("Retired %s: %d index objects and %d records deleted.\n", scope, len(keys), len(pkgs))
	return nil
}

func runClone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ExitOnError)
	var f flags
	f.bind(fs)
	fromArg := fs.String("from", "", "version to copy packages from, as <distro>/<version>")
	toArg := fs.String("to", "", "version to copy packages into, as <distro>/<version>")
	confirm := fs.Bool("confirm", false, "actually copy, rather than reporting what would be copied")
	if err := fs.Parse(args); err != nil {
		return err
	}

	from, err := clone.ParseVersion(*fromArg)
	if err != nil {
		return err
	}
	to, err := clone.ParseVersion(*toArg)
	if err != nil {
		return err
	}

	clients, err := f.connect(ctx)
	if err != nil {
		return err
	}
	cfg, err := clients.LoadRepoConfig(ctx)
	if err != nil {
		return err
	}

	pairs, err := clone.Trees(cfg, from, to)
	if err != nil {
		return err
	}

	copier := &bucketCopier{clients: clients, bucket: f.bucket}
	plans, err := clone.PlanAll(ctx, copier, pairs)
	if err != nil {
		return err
	}

	var total int
	for _, p := range plans {
		total += len(p.Copies)
		detail := fmt.Sprintf("%d to copy", len(p.Copies))
		if p.Note != "" {
			detail += " (" + p.Note + ")"
		}
		fmt.Printf("%-28s -> %-28s %s\n", p.FromDir, p.ToDir, detail)
	}
	fmt.Printf("\n%d objects across %d trees.\n", total, len(plans))

	if total == 0 {
		fmt.Println("Nothing to do.")
		return nil
	}
	// No interactive prompt, unlike retire: this only adds objects, and adding
	// the same package twice is a no-op. Carrying a version forward belongs in
	// CI, where there is nobody to answer one.
	if !*confirm {
		fmt.Println("\nNothing copied. Re-run with --confirm to proceed.")
		return nil
	}

	copied, err := clone.Execute(ctx, copier, plans)
	if err != nil {
		return fmt.Errorf("%w (%d objects copied before the failure)", err, copied)
	}

	fmt.Printf("\nCopied %d objects into %s.\n\n", copied, to)
	fmt.Print(`Each copy is an S3 event, so ingest is still catching up. To watch it drain:

  aws sqs get-queue-attributes --queue-url "$INGEST_QUEUE_URL" \
    --attribute-names ApproximateNumberOfMessages

When the queue is empty, confirm every record resolves to an object:

`)
	for _, p := range plans {
		if len(p.Copies) > 0 {
			fmt.Printf("  seed verify --scope %s\n", p.To)
		}
	}
	return nil
}

// bucketCopier is object storage as the clone needs it: server-side copies, so
// the packages never travel through this machine.
type bucketCopier struct {
	clients *awsx.Clients
	bucket  string
}

func (b *bucketCopier) List(ctx context.Context, prefix string) ([]string, error) {
	return listUnder(ctx, b.clients, b.bucket, prefix)
}

func (b *bucketCopier) Copy(ctx context.Context, srcKey, dstKey string) error {
	_, err := b.clients.S3.Client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(b.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(url.PathEscape(b.bucket + "/" + srcKey)),
	})
	return err
}

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var f flags
	f.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	clients, err := f.connect(ctx)
	if err != nil {
		return err
	}
	cfg, err := clients.LoadRepoConfig(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("%-40s %10s %10s %s\n", "SCOPE", "GEN", "PUBLISHED", "STATE")
	for _, scope := range cfg.PublishScopes() {
		st, err := clients.State.Get(ctx, scope)
		if err != nil {
			return err
		}
		state := "clean"
		if st.Dirty() {
			state = "DIRTY"
		}
		if st.LeaseOwner != "" {
			state += " (leased by " + st.LeaseOwner + ")"
		}
		fmt.Printf("%-40s %10d %10d %s\n", scope, st.Generation, st.PublishedGeneration, state)
	}
	return nil
}

// publishedDir returns the directory a scope's index occupies. A deb pool has
// no directory of its own — its files live under every codename — so retiring
// one is not meaningful and is refused.
func publishedDir(cfg *repoconfig.Config, scope repoconfig.Scope) (string, error) {
	switch scope.Kind {
	case repoconfig.KindDebRelease:
		return cfg.CodenameDir(scope.Codename), nil
	case repoconfig.KindRPMTree:
		return cfg.TreeDir(scope), nil
	case repoconfig.KindDebPool:
		return "", errors.New("a deb pool scope publishes into every codename and has no tree of its own; retire the codename instead")
	default:
		return "", fmt.Errorf("cannot retire a %s scope", scope.Kind)
	}
}

func listUnder(ctx context.Context, clients *awsx.Clients, bucket, dir string) ([]string, error) {
	prefix := strings.TrimSuffix(dir, "/") + "/"

	var (
		keys  []string
		token *string
	)
	for {
		page, err := clients.S3.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
		if !aws.ToBool(page.IsTruncated) {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

// askForConfirmation requires the scope to be typed out, so a destructive
// operation cannot be completed by holding down the return key.
func askForConfirmation(scope string) bool {
	fmt.Printf("Type the scope to confirm deletion (%s): ", scope)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(line) == scope
}

func chunks[T any](s []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(s); start += size {
			if !yield(s[start:min(start+size, len(s))]) {
				return
			}
		}
	}
}
