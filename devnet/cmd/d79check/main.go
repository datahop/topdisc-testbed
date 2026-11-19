package main

import (
	"fmt"
	"os"
	"sort"
)

type command struct {
	usage string
	run   func(args []string) error
}

var commands = map[string]command{}

func register(name, usage string, run func([]string) error) {
	commands[name] = command{usage, run}
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]].run == nil {
		names := make([]string, 0, len(commands))
		for n := range commands {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(os.Stderr, "usage: d79check <command> [flags]")
		for _, n := range names {
			fmt.Fprintf(os.Stderr, "  %-12s %s\n", n, commands[n].usage)
		}
		os.Exit(2)
	}
	if err := commands[os.Args[1]].run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "d79check:", err)
		os.Exit(1)
	}
}
