// %T and %#v on interpreted types, and reflect on them. Models use %T when
// debugging type assertions; the output leaks the interpreter's type naming.
package main

import (
	"fmt"
	"reflect"
)

type Pod struct {
	Name string
	CPU  int
}

type Phase string

func main() {
	p := Pod{"web", 2}
	fmt.Printf("%T %T %T\n", p, &p, []Pod{})
	fmt.Printf("%#v\n", p)
	var ph Phase = "Running"
	fmt.Printf("%T %v %#v\n", ph, ph, ph)

	t := reflect.TypeOf(p)
	fmt.Println(t.Name(), t.Kind(), t.NumField())
	for i := 0; i < t.NumField(); i++ {
		fmt.Println(t.Field(i).Name, t.Field(i).Type)
	}
	fmt.Println(reflect.ValueOf(p).Field(1).Int())
}
