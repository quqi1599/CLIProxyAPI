package payload

// RawJSON is a JSON fragment that can be written into an array without encoding.
type RawJSON interface {
	~string | ~[]byte
}

// BuildRaw builds a JSON array from already-encoded items in one allocation.
func BuildRaw[T RawJSON](items []T) []byte {
	size := 2
	if len(items) > 1 {
		size += len(items) - 1
	}
	for _, item := range items {
		size += len(item)
	}

	out := make([]byte, 0, size)
	out = append(out, '[')
	for idx, item := range items {
		if idx > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}
