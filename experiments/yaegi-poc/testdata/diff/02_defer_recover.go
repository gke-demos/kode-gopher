// defer ordering, recover into a named error result, defer modifying results.
package main

import (
	"errors"
	"fmt"
)

func safeDiv(a, b int) (res int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered: %v", r)
		}
	}()
	return a / b, nil
}

func counted() (n int) {
	defer func() { n *= 10 }()
	n = 4
	return n + 1
}

func order() {
	for i := 0; i < 3; i++ {
		defer fmt.Println("deferred", i)
	}
	fmt.Println("body done")
}

func cleanup() (err error) {
	defer func() {
		if cerr := errors.New("close failed"); err == nil {
			err = cerr
		}
	}()
	return nil
}

func main() {
	for _, b := range []int{2, 0, 5} {
		fmt.Println(safeDiv(10, b))
	}
	fmt.Println(counted())
	order()
	fmt.Println(cleanup())
}
