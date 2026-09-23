package backend

// Batch wraps emit so the updates one observation produces go out as a
// batch. The returned add holds each update until the next one arrives, then
// passes it to emit with More set. flush passes the held update without More,
// which closes the batch. Call flush once per observation, after its last add.
func Batch(emit func(Update)) (add func(Update), flush func()) {
	var held *Update
	add = func(u Update) {
		if held != nil {
			prev := *held
			prev.More = true
			emit(prev)
		}
		held = &u
	}
	flush = func() {
		if held != nil {
			last := *held
			last.More = false
			held = nil
			emit(last)
		}
	}
	return add, flush
}
