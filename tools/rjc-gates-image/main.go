// Package main prepares the rjc-gates runner image: it creates the NOCOW
// image and migrates an existing one on btrfs once. It never preallocates.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

const (
	defaultImage     = "/var/lib/rjc.img"
	defaultMount     = "/var/lib/rjc"
	defaultMkfs      = "mkfs.ext4"
	defaultMountinfo = "/proc/self/mountinfo"
	// gibibyte is the unit of --size-gib.
	gibibyte = int64(1) << 30
)

func newRootCommand(stdout, stderr io.Writer) (*cobra.Command, *int) {
	var (
		image    = defaultImage
		mount    = defaultMount
		mkfs     = defaultMkfs
		sizeGiB  int64
		mountTab = defaultMountinfo
	)
	code := new(int)
	command := &cobra.Command{
		Use:           "rjc-gates-image",
		Short:         "Create and migrate the rjc-gates runner image",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if sizeGiB <= 0 {
				*code = 1
				return fmt.Errorf("rjc-gates-image: --size-gib must be positive, got %d", sizeGiB)
			}
			cfg := config{
				image:     image,
				temporary: image + ".tmp",
				mount:     mount,
				size:      sizeGiB * gibibyte,
			}
			fsys := realFS{mkfsPath: mkfs, mountinfo: mountTab}
			log := func(message string) {
				if _, err := fmt.Fprintf(stderr, "rjc-gates-image: %s\n", message); err != nil {
					*code = 1
				}
			}
			if err := prepare(fsys, cfg, log); err != nil {
				*code = 1
				return fmt.Errorf("rjc-gates-image: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&image, "image", defaultImage, "Image file to create or migrate.")
	command.Flags().StringVar(&mount, "mount", defaultMount, "Mount point of the image.")
	command.Flags().StringVar(&mkfs, "mkfs", defaultMkfs, "Path to the mkfs.ext4 binary.")
	command.Flags().Int64Var(&sizeGiB, "size-gib", 0, "Image size in GiB.")
	command.Flags().StringVar(&mountTab, "mountinfo", defaultMountinfo, "Mount table to read.")
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.CompletionOptions.DisableDefaultCmd = true
	return command, code
}

func run(args []string, stdout, stderr io.Writer) int {
	command, code := newRootCommand(stdout, stderr)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		if *code == 0 {
			return 2
		}
	}
	return *code
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
