package main

import (
	"os"

	"github.com/PacketCipher/hiddify-core/cmd"
)

func main() {
	cmd.ParseCli(os.Args[1:])
}
