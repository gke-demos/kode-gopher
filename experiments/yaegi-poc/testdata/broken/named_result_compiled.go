// Named result of a compiled struct type, mutated via a pointer-receiver method.
package main

import (
	"bytes"
	"fmt"
)

func f() (b bytes.Buffer) {
	b.WriteString("x")
	return
}

func main() {
	for i := 0; i < 3; i++ {
		r := f()
		fmt.Printf("call %d: %q\n", i, r.String())
	}
}
