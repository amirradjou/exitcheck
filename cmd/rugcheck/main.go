// Command rugcheck runs the rugcheck watcher, analyzer and Telegram bot (see README.md).
package main

import (
	"fmt"
	"os"

	"github.com/amirradjou/rugcheck/internal/buildinfo"
)

const usage = `usage: rugcheck <command>

commands:
  version   print the version
`

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(buildinfo.String())
		return
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
