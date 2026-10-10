package durability

import "testing"

func TestWrittenRow_HasValidShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		row  WrittenRow
		want bool
	}{
		{"ordinary", WrittenRow{Offset: 10, Length: 5}, true},
		{"zero length", WrittenRow{Offset: 10, Length: 0}, true},
		{"zero offset", WrittenRow{Offset: 0, Length: 1}, true},
		{"negative offset", WrittenRow{Offset: -1, Length: 5}, false},
		{"negative length", WrittenRow{Offset: 10, Length: -1}, false},
	} {
		if got := tc.row.HasValidShape(); got != tc.want {
			t.Errorf("%s: HasValidShape = %v, want %v", tc.name, got, tc.want)
		}
	}
}
