// Type switches and assertions over interpreted and builtin types.
package main

import (
	"fmt"
	"strconv"
)

type Shape interface{ Area() float64 }

type Rect struct{ W, H float64 }
type Circle struct{ R float64 }

func (r Rect) Area() float64    { return r.W * r.H }
func (c *Circle) Area() float64 { return 3 * c.R * c.R }

func describe(v any) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case int, int64:
		return fmt.Sprintf("integer %v", x)
	case string:
		return "string " + strconv.Quote(x)
	case Rect:
		return fmt.Sprintf("rect %.0f", x.Area())
	case Shape:
		return fmt.Sprintf("shape %.0f", x.Area())
	case error:
		return "error " + x.Error()
	case []any:
		return fmt.Sprintf("slice of %d", len(x))
	default:
		return fmt.Sprintf("other %T", x)
	}
}

func main() {
	vals := []any{nil, 3, int64(4), "hi", Rect{2, 3}, &Circle{2}, fmt.Errorf("e"), []any{1, 2}, 2.5}
	for _, v := range vals {
		fmt.Println(describe(v))
	}
	var s Shape = Rect{1, 1}
	if r, ok := s.(Rect); ok {
		fmt.Println("is rect", r.W)
	}
	if _, ok := s.(*Circle); !ok {
		fmt.Println("not circle")
	}
}
