// Command repoadmin performs the destructive operations the service will not
// do on its own.
//
// Removing a distribution from repos.yaml stops it being published but leaves
// its tree in place, so a typo in the config cannot delete a live repository.
// Actually tearing one down is this command's job, and it asks first.
//
//	repoadmin retire --bucket b --scope rpm/RHEL/8/x86_64/stable --confirm
//	repoadmin status --bucket b
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ashleyconnor/linux-repo-indexer/internal/awsx"
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

retire deletes a scope's published index and its stored records. It does not
delete package objects: those are the only copy of themselves.
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

// askForConfirmation requires the scope to be typed out, so this cannot be
// completed by holding down the return key.
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
