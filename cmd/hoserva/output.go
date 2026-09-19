package main

import (
	"encoding/json"
	"fmt"
	"os"
)

const (
	exitOK     = 0
	exitError  = 1
	exitDoctor = 4
)

var jsonOutput bool

func emit(v any) {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "hoserva: encoding output: %v\n", err)
			os.Exit(exitError)
		}
		return
	}
	printHuman(v)
}

func printHuman(v any) {
	switch x := v.(type) {
	case string:
		fmt.Println(x)
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	}
}
