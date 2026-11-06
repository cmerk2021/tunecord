// Command tunecord shows the current MPRIS track as a Discord Rich Presence.
package main

import (
	"context"
	"os"

	"tunecord/internal/cli"
)

func main() {
	os.Exit(cli.Execute(context.Background()))
}
