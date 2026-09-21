// Command pipex runs the text pipeline over a file's contents.
package main

import (
	"fmt"
	"os"
	"strings"

	"pipex/internal/pipe"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pipex <file>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := pipe.New(pipe.Config{})
	for _, tok := range p.Run([]string{string(data)}) {
		fmt.Println(strings.TrimSpace(tok))
	}
}
