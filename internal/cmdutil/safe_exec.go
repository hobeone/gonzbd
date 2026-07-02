package cmdutil

import "fmt"

// SafeEngineRun executes fn, trapping any panic and converting it to a formatted error
// with the given prefix. If onPanic callbacks are provided, they are invoked in order
// with the recovered panic value before returning the error.
func SafeEngineRun(prefix string, fn func() error, onPanic ...func(p any)) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s: %v", prefix, p)
			for _, callback := range onPanic {
				if callback != nil {
					callback(p)
				}
			}
		}
	}()
	return fn()
}
