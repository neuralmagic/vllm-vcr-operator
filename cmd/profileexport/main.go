// Command profileexport exports recorded profile content into a Pod volume.
//
// A profile image declares this command (or an equivalent) as
// provisioning.entrypoint. The operator runs it as the vcr-profile-provisioner
// init container with a single argument, the target directory:
//
//	vcr-profile-export <target-dir>
//
// It reads the image filesystem (--source, default /profile), verifies the
// metadata document against the profile image contract, verifies that the
// trace file the metadata declares is present and non-empty, then replaces the
// target directory with a staged copy of both documents.
//
// The replacement is atomic at the file level: content is written into a staging
// directory inside the target (the profile volume is the only writable path in the
// provisioner container) and each document is moved into place only after every
// check has passed, with metadata.json published last. A failed export therefore
// never leaves a target directory that serving could mistake for a validated
// profile, and every path removes the staging directory. On success the command
// prints a single-line JSON summary on stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// defaultSourceDir is the profile image filesystem directory defined by
// docs/contracts/profile-image.md.
const defaultSourceDir = "/profile"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command entrypoint: arguments in, exit code, streams and
// filesystem effects out.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vcr-profile-export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", defaultSourceDir, "profile image filesystem directory to export from")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "usage: vcr-profile-export [--source DIR] TARGET_DIR\n\n")
		fmt.Fprintf(stderr, "Verify the profile metadata and trace in --source and copy both into TARGET_DIR.\n\n")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}

	result, err := export(ctx, *source, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "vcr-profile-export: %v\n", err)
		return 1
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(stderr, "vcr-profile-export: encode summary: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}
