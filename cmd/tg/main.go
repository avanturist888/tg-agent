// tg — tg-agent: служба, MCP-прослойка агентов, CLI и окно управления.
package main

import (
	"os"

	"tgagent/internal/cli"
	"tgagent/internal/gui"
)

func main() {
	cli.GUI = gui.Open
	os.Exit(cli.Main(os.Args[1:]))
}
