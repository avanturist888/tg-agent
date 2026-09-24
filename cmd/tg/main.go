// tg — tg-agent на Go: MCP-сервер, слушатель кнопок, CLI и окно управления.
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
