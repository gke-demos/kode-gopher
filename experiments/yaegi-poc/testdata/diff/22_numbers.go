// Numeric edge cases: integer overflow wraparound, conversions, iota with
// expressions, bit ops, float formatting, division semantics.
package main

import (
	"fmt"
	"math"
	"strconv"
)

type ByteSize float64

const (
	_           = iota
	KB ByteSize = 1 << (10 * iota)
	MB
	GB
)

type Weekday int

const (
	Sunday Weekday = iota
	Monday
	Tuesday
)

func main() {
	var i8 int8 = 127
	i8++
	var u8 uint8 = 0
	u8--
	fmt.Println(i8, u8)

	fmt.Println(KB, MB, GB, Tuesday)
	fmt.Println(-7/2, -7%2, 7>>1, 1<<10, 0b1010&0b0110, 0x0f|0xf0, 6^3, 6&^3)

	f := 2.0 / 3.0
	fmt.Println(f, float32(f), int(f*100), math.Round(f*100)/100)
	fmt.Printf("%.2f %8.3f %e %g\n", f, f, 123456.789, 1e21)
	fmt.Println(math.MaxInt64, math.Inf(1), -math.Inf(1), math.IsNaN(math.NaN()))

	n, err := strconv.Atoi("12x")
	fmt.Println(n, err)
	u, _ := strconv.ParseUint("18446744073709551615", 10, 64)
	fmt.Println(u, strconv.FormatInt(-255, 16))

	var bytes int64 = 3*1024*1024*1024 + 512
	fmt.Printf("%.1f GiB\n", float64(bytes)/float64(1<<30))
}
