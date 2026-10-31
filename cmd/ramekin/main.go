// ramekin is the developer CLI: build a SIF from an Apptainer definition file or a
// persistent overlay, scan it for CVEs, push it to a test device, and publish
// it signed and encrypted to GitHub Releases for the fleet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

type command struct {
	name    string
	args    string
	summary string
	run     func(ctx context.Context, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"init", "[dir]", "scaffold a new project", cmdInit},
		{"keys", "generate", "create the signing keypair and fleet encryption key", cmdKeys},
		{"overlay", "init [--force]", "overlay mode: pull the base image and create overlay.img", cmdOverlay},
		{"shell", "", "overlay mode: open a shell whose changes persist in overlay.img", cmdShell},
		{"build", "", "build the SIF and scan it for CVEs", cmdBuild},
		{"scan", "[sif]", "scan a SIF for CVEs and apply the project's policy", cmdScan},
		{"sign", "[sif]", "sign a SIF with the project key", cmdSign},
		{"verify", "[sif]", "verify a SIF's signature with the project public key", cmdVerify},
		{"device", "init|claim|set|update|find|list", "create, claim, configure, update and locate gokrazy devices", cmdDevice},
		{"push", "<device> [sif]", "sign and push a SIF to a device for testing", cmdPush},
		{"logs", "<device> [-f]", "show a device's container and agent logs", cmdLogs},
		{"status", "<device>", "show what a device is running", cmdStatus},
		{"restart", "<device>", "restart a device's container", cmdRestart},
		{"rollback", "<device> [version]", "switch a device to its previous (or an earlier kept) release, health-checked first", cmdRollback},
		{"publish", "--version V [--channel C]", "scan, sign, encrypt and upload a release, then update a channel", cmdPublish},
		{"promote", "<version> --channel C", "point a channel at an already-published version (also used to roll back)", cmdPromote},
		{"releases", "", "list published releases and channels", cmdReleases},
		{"firmware", "init|publish|push", "build the fleet OS image and publish or push it (kernel, gokrazy, ramekind)", cmdFirmware},
		{"watch", "[--interval D]", "rescan published releases against fresh CVE data and alert", cmdWatch},
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ramekin <command> [arguments]\n       ramekin --dashboard [--listen ADDR] [--no-open]\n\ncommands:")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-9s %-28s %s\n", c.name, c.args, c.summary)
	}
	fmt.Fprintf(os.Stderr, "\n  --dashboard  serve a web dashboard of the fleet's health and resource use, with\n               version switching (devices in .ramekin/devices; 127.0.0.1:8099)\n")
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if os.Args[1] == "--dashboard" {
		commands = append(commands, command{name: "--dashboard", run: cmdDashboard})
	}
	for _, c := range commands {
		if c.name == os.Args[1] {
			err := c.run(ctx, os.Args[2:])
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(2)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "ramekin %s: %v\n", c.name, err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "ramekin: unknown command %q\n\n", os.Args[1])
	usage()
	os.Exit(2)
}

// flags creates a FlagSet that tolerates flags after positional arguments.
func flags(name string) *flag.FlagSet {
	return flag.NewFlagSet("ramekin "+name, flag.ContinueOnError)
}

// parse parses args allowing interspersed flags and positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }
