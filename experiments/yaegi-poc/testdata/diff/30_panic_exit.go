// An unrecovered panic after some output: Go exits 2 with the output so far
// on stdout. The interpreter should fail too, not report success.
package main

import "fmt"

type cfg struct{ replicas map[string]int }

func main() {
	fmt.Println("before")
	var c cfg
	c.replicas["web"] = 3 // assignment to entry in nil map
	fmt.Println("after")
}
