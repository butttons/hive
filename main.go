package main

import (
	"context"
	"fmt"
	"os"
)

type command struct {
	name    string
	summary string
	run     func(context.Context, []string) error
}

var commands = []command{
	{"add", "Scaffold a new celld app in the current directory", cmdAdd},
	{"deploy", "Build, ship to the fleet bucket, restart the node, wait for healthy", cmdDeploy},
	{"up", "Start the node for this app (process, or docker with --docker)", cmdUp},
	{"down", "Stop the node gracefully", cmdDown},
	{"status", "Show what is running, which version, and health", cmdStatus},
	{"init", "Configure bucket credentials for this project", cmdInit},
	{"bootstrap", "Install hive + celld on the app's server", cmdBootstrap},
	{"cf", "Cloudflare commands: login, tunnel", cmdCF},
	{"exe", "exe.dev commands: new, share, domain", cmdExe},
	{"ui", "Serve the local fleet dashboard", cmdUI},
}

func usage() {
	fmt.Println("hive — a wrangler-shaped toolchain for celld fleets")
	fmt.Println()
	fmt.Println("usage: hive <command> [flags]")
	fmt.Println()
	for _, c := range commands {
		fmt.Printf("  %-8s %s\n", c.name, c.summary)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage()
		return
	}
	for _, c := range commands {
		if c.name == name {
			if err := c.run(context.Background(), os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "hive %s: %v\n", name, err)
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", name)
	usage()
	os.Exit(1)
}

func notImplemented(name string) error {
	return fmt.Errorf("not implemented yet")
}
