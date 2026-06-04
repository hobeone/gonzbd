package postproc

import (
	"reflect"
	"testing"
)

func TestToolOutputLinesDirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "empty",
			input: "",
			want:  []string{},
		},
		{
			name:  "whitespace only lines",
			input: "  \n\t\n \r\n",
			want:  []string{},
		},
		{
			name:  "mixed valid and empty lines",
			input: "line1\n\nline2\r\n  \nline3 \t\r\n",
			want:  []string{"line1", "line2", "line3"},
		},
		{
			name:  "single line no newline",
			input: "single",
			want:  []string{"single"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolOutputLines(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("toolOutputLines(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
