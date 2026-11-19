package ui

// Go runs fn on a new goroutine and hands a panic in it to onPanic instead of
// letting it end the process with the terminal left raw. It holds the
// console's only go statement, so every goroutine has this recover.
func Go(fn func(), onPanic func(v any)) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				onPanic(v)
			}
		}()
		fn()
	}()
}
