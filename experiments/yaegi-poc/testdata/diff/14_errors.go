// Error handling: wrapping with %w, errors.Is/As against interpreted error
// types and sentinels, errors.Join.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

var ErrQuota = errors.New("quota exceeded")

type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("api %d: %s", e.Code, e.Msg) }

func call(i int) error {
	switch i {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("listing buckets: %w", ErrQuota)
	case 2:
		return fmt.Errorf("get: %w", &APIError{404, "missing"})
	default:
		_, err := os.Open("/definitely/not/here")
		return fmt.Errorf("open: %w", err)
	}
}

func main() {
	for i := 0; i < 4; i++ {
		err := call(i)
		var apiErr *APIError
		fmt.Println(i, err,
			errors.Is(err, ErrQuota),
			errors.As(err, &apiErr) && apiErr.Code == 404,
			errors.Is(err, fs.ErrNotExist))
	}
	j := errors.Join(ErrQuota, &APIError{500, "boom"})
	var apiErr *APIError
	fmt.Println(j, "|", errors.Is(j, ErrQuota), errors.As(j, &apiErr), apiErr.Code)
}
