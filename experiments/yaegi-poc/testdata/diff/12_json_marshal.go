// encoding/json over interpreted structs: tags, omitempty, nested, embedded,
// pointer fields, maps, and a custom MarshalJSON.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Meta struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type Level int

func (l Level) MarshalJSON() ([]byte, error) {
	return json.Marshal(strings.Repeat("*", int(l)))
}

type Cluster struct {
	Meta
	Name     string   `json:"name"`
	Zone     string   `json:"zone,omitempty"`
	Nodes    int      `json:"nodeCount"`
	Tags     []string `json:"tags"`
	Owner    *string  `json:"owner"`
	Priority Level    `json:"priority"`
	internal string
}

func main() {
	owner := "sre"
	cs := []Cluster{
		{Name: "a", Nodes: 3, Tags: []string{"prod"}, Owner: &owner, Priority: 2,
			Meta: Meta{Labels: map[string]string{"env": "prod", "app": "x"}}},
		{Name: "b", Zone: "us-central1-a", internal: "hidden"},
	}
	b, err := json.Marshal(cs)
	fmt.Println(string(b), err)

	b, _ = json.MarshalIndent(map[string]any{"count": len(cs), "first": cs[0].Name}, "", "  ")
	fmt.Println(string(b))
}
