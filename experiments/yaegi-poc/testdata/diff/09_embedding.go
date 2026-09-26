// Struct embedding: promoted fields and methods, overriding, interface
// satisfaction through an embedded type.
package main

import "fmt"

type Named interface{ Name() string }

type Base struct{ ID string }

func (b Base) Name() string   { return "base:" + b.ID }
func (b *Base) Rename(s string) { b.ID = s }

type Cluster struct {
	Base
	Nodes int
}

type Special struct {
	Base
}

func (s Special) Name() string { return "special:" + s.ID }

func describe(n Named) string { return n.Name() }

func main() {
	c := Cluster{Base: Base{ID: "c1"}, Nodes: 3}
	fmt.Println(c.ID, c.Name(), describe(c))
	c.Rename("c2")
	fmt.Println(c.ID, c.Base.Name(), describe(&c))

	s := Special{Base{"s1"}}
	fmt.Println(s.Name(), s.Base.Name(), describe(s))

	items := []Named{c, s, Base{"b"}}
	for _, it := range items {
		fmt.Println(it.Name())
	}
}
