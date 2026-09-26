// Interpreted io.Reader / io.Writer implementations passed to compiled code.
package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type upper struct{ r io.Reader }

func (u upper) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	for i := 0; i < n; i++ {
		if p[i] >= 'a' && p[i] <= 'z' {
			p[i] -= 32
		}
	}
	return n, err
}

type countWriter struct {
	n     int
	lines int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	c.lines += strings.Count(string(p), "\n")
	return len(p), nil
}

func main() {
	b, err := io.ReadAll(upper{strings.NewReader("hello, gke\nsecond line\n")})
	fmt.Printf("%q %v\n", b, err)

	cw := &countWriter{}
	fmt.Fprintf(cw, "a=%d\nb=%d\n", 1, 2)
	io.Copy(cw, strings.NewReader("x\ny\nz"))
	fmt.Println(cw.n, cw.lines)

	sc := bufio.NewScanner(upper{strings.NewReader("one two\nthree")})
	sc.Split(bufio.ScanWords)
	var words []string
	for sc.Scan() {
		words = append(words, sc.Text())
	}
	fmt.Println(words)
}
