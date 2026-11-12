// tg — tg-agent: служба, MCP-прослойка агентов, CLI и окно управления.
package main

import (
	"os"

	"tgagent/internal/cli"
	"tgagent/internal/gui"
	"tgagent/internal/store"
)

func main() {
	cli.GUI = gui.Open
	// старые JSON-файлы переносить только после tg cutover: рядом может
	// работать прежняя сборка (store.CutoverFile)
	store.RequireCutover = true
	os.Exit(cli.Main(os.Args[1:]))
}
