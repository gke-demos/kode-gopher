// fmt on interpreted types: Stringer (value and pointer receivers), error
// types, %v/%+v/%#v on nested structs, Formatter-free defaults.
package main

import (
	"fmt"
)

type Color int

const (
	Red Color = iota
	Green
	Blue
)

func (c Color) String() string { return [...]string{"Red", "Green", "Blue"}[c] }

type Point struct{ X, Y int }

func (p *Point) String() string { return fmt.Sprintf("P(%d,%d)", p.X, p.Y) }

type Node struct {
	Name   string
	Labels map[string]string
	Color  Color
	Pos    Point
}

type NotFound struct{ Key string }

func (e NotFound) Error() string { return "not found: " + e.Key }

func main() {
	fmt.Println(Green, []Color{Red, Blue})
	fmt.Printf("%v %d %s\n", Blue, Blue, Blue)

	p := Point{1, 2}
	fmt.Println(p, &p)

	n := Node{Name: "n1", Labels: map[string]string{"b": "2", "a": "1"}, Color: Blue, Pos: Point{3, 4}}
	fmt.Printf("%v\n%+v\n", n, n)

	var err error = NotFound{"x"}
	fmt.Println(err)
	fmt.Printf("%v | %s | %q\n", err, err, err)
	fmt.Println(fmt.Sprint(3, "a", 4, 5, "b"))
}
