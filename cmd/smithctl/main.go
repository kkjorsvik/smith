// Command smithctl is the smith operator CLI. It resolves a directory of GitOps
// app bundles and applies the resulting resources to the control-plane API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kkjorsvik/smith/internal/apply"
	"github.com/kkjorsvik/smith/internal/client"
	"github.com/kkjorsvik/smith/internal/secrets"
)

const usage = "usage: smithctl [--config PATH] apply [--dry-run] <dir>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "smithctl: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("smithctl", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the smithctl config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New(usage)
	}
	switch rest[0] {
	case "apply":
		return runApply(*configPath, rest[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", rest[0], usage)
	}
}

func runApply(configPath string, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "validate and print a plan without applying")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl apply [--dry-run] <dir>")
	}
	dir := fs.Arg(0)

	if configPath == "" {
		p, err := client.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = p
	}
	cfg, err := client.LoadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Apply(dir, client.New(cfg), secrets.SopsDecryptor{}, *dryRun, os.Stdout)
}
