// Plain int named result, mutated.
package main

import "fmt"

func f() (n int) {
	n++
	return
}

func main() {
	for i := 0; i < 3; i++ {
		fmt.Printf("call %d: %d\n", i, f())
	}
}
