// Same as named_result_compiled.go but with a local var instead of a named result.
package main

import (
	"bytes"
	"fmt"
)

func f() bytes.Buffer {
	var b bytes.Buffer
	b.WriteString("x")
	return b
}

func main() {
	for i := 0; i < 3; i++ {
		r := f()
		fmt.Printf("call %d: %q\n", i, r.String())
	}
}
