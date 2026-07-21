package payload

import "testing"

func TestBuildRaw(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{name: "empty bytes", got: BuildRaw([][]byte(nil)), want: "[]"},
		{name: "bytes", got: BuildRaw([][]byte{[]byte(`{"id":1}`), []byte(`null`)}), want: `[{"id":1},null]`},
		{name: "strings", got: BuildRaw([]string{`"a"`, `{"id":2}`}), want: `["a",{"id":2}]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Fatalf("BuildRaw() = %s, want %s", tt.got, tt.want)
			}
			if len(tt.got) != cap(tt.got) {
				t.Fatalf("BuildRaw() capacity = %d, want %d", cap(tt.got), len(tt.got))
			}
		})
	}
}
