package main

import (
	"fmt"
	"os"

	"github.com/webitel/webitel-emails/cmd"
)

func main() {
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
