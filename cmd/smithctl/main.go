// Command smithctl is the smith operator CLI. It resolves a directory of GitOps
// app bundles and applies (or diffs/prunes) them against the control-plane API.
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

const usage = "usage: smithctl [--config PATH] <apply|diff> [flags] <dir>"

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
	case "diff":
		return runDiff(*configPath, rest[1:])
	default:
		return fmt.Errorf("unknown command %q\n%s", rest[0], usage)
	}
}

func runApply(configPath string, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "validate and print a plan without applying")
	prune := fs.Bool("prune", false, "delete live resources not declared in <dir>")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl apply [--dry-run] [--prune] <dir>")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Apply(fs.Arg(0), client.New(cfg), secrets.SopsDecryptor{}, *dryRun, *prune, os.Stdout)
}

func runDiff(configPath string, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: smithctl diff <dir>")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	return apply.Diff(fs.Arg(0), client.New(cfg), os.Stdout)
}

// loadConfig resolves the config path (defaulting when empty) and loads it.
func loadConfig(configPath string) (client.Config, error) {
	if configPath == "" {
		p, err := client.DefaultConfigPath()
		if err != nil {
			return client.Config{}, err
		}
		configPath = p
	}
	return client.LoadConfig(configPath)
}
