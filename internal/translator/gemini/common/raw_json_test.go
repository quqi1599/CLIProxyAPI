package common

import "testing"

func TestRawJSONArrayPreservesOrderAndRawFields(t *testing.T) {
	got := string(RawJSONArray([][]byte{
		[]byte(`{"known":1,"unknown":{"x":true}}`),
		[]byte(`"second"`),
	}))
	want := `[{"known":1,"unknown":{"x":true}},"second"]`
	if got != want {
		t.Fatalf("RawJSONArray() = %s, want %s", got, want)
	}
}
