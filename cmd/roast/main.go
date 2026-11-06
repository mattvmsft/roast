// Command roast is an adversarial LLM code reviewer: a finder proposes
// findings, then an independent verifier adjudicates each claim against the
// actual code.
package main

import (
	"os"

	"github.com/bluesky-social/roast/internal/cli"
)

func main() {
	code, err := cli.Execute()
	cli.WriteError(os.Stderr, code, err)
	os.Exit(code)
}
