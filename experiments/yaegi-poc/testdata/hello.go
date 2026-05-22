// Stdlib-only baseline. If yaegi can't run this, the experiment is over.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	result := map[string]any{
		"message": "hello from yaegi",
		"answer":  42,
		"items":   []string{"a", "b", "c"},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
}
