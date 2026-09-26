// os.Exit with a nonzero code after output — how snippets signal failure.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("partial result")
	defer fmt.Println("deferred: must NOT print, os.Exit skips defers")
	os.Exit(3)
}
