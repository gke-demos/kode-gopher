// Method values and expressions, methods on non-struct types, pointer vs
// value receivers through interfaces and addressable values.
package main

import (
	"fmt"
	"strings"
)

type Counter struct{ n int }

func (c *Counter) Inc()      { c.n++ }
func (c Counter) Get() int   { return c.n }

type Path []string

func (p Path) String() string { return "/" + strings.Join(p, "/") }
func (p *Path) Push(s string) { *p = append(*p, s) }

type Op func(int) int

func (o Op) Twice(x int) int { return o(o(x)) }

func apply(fs []func(), n int) {
	for i := 0; i < n; i++ {
		for _, f := range fs {
			f()
		}
	}
}

func main() {
	var c Counter
	inc := c.Inc // method value binds &c
	apply([]func(){inc, inc}, 3)
	fmt.Println(c.Get())

	get := Counter.Get      // method expression
	incE := (*Counter).Inc
	incE(&c)
	fmt.Println(get(c))

	var p Path
	p.Push("apis")
	p.Push("v1")
	fmt.Println(p, len(p))

	addOne := Op(func(x int) int { return x + 1 })
	fmt.Println(addOne.Twice(5))

	counters := []Counter{{1}, {2}}
	for i := range counters {
		counters[i].Inc()
	}
	fmt.Println(counters)
}
