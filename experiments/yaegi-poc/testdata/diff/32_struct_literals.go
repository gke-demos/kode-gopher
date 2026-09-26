// Composite literals: nested anonymous structs, map/slice literals with
// elided types, pointers to literals, zero values, struct copying in range.
package main

import "fmt"

type Container struct {
	Name  string
	Ports []int
	Env   map[string]string
	Res   struct {
		CPU, Mem string
	}
}

type Pod struct {
	Name       string
	Containers []Container
	Owner      *Pod
}

func main() {
	p := Pod{
		Name: "web",
		Containers: []Container{
			{Name: "app", Ports: []int{8080}, Env: map[string]string{"MODE": "prod"}},
			{Name: "sidecar"},
		},
	}
	p.Containers[0].Res.CPU = "500m"
	child := &Pod{Name: "child", Owner: &p}
	fmt.Println(child.Owner.Name, child.Owner.Containers[0].Res.CPU)

	for _, c := range p.Containers {
		fmt.Println(c.Name, c.Ports == nil, c.Env == nil, len(c.Env), c.Res.CPU == "")
	}

	points := []struct{ X, Y int }{{1, 2}, {3, 4}}
	lookup := map[string][]*Pod{"a": {&p}, "b": {child, &p}}
	fmt.Println(points, len(lookup["b"]), lookup["b"][0].Name)

	matrix := [2][3]int{{1, 2, 3}, {4, 5, 6}}
	fmt.Println(matrix, len(matrix[1]))

	var zero Pod
	fmt.Printf("%q %v %v\n", zero.Name, zero.Containers == nil, zero.Owner == nil)
}
