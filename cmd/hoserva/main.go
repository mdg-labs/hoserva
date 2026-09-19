// Command hoserva is the Hoserva CLI (doc 01 §3). It speaks to hoservad
// over the Unix socket API through the generated Go client (D5, D18).
package main

import "os"

func main() {
	os.Exit(runCLI())
}
