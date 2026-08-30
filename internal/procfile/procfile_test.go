package procfile

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	input := `# local services

web: unset PORT && bin/rails server
js: yarn build --watch
worker: bundle exec sidekiq -C config/sidekiq.yml
`
	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(Parse()) = %d, want 3", len(got))
	}
	if got[0].Name != "web" || got[0].Command != "unset PORT && bin/rails server" {
		t.Fatalf("first process = %#v", got[0])
	}
}

func TestParseRejectsInvalidAndDuplicateEntries(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing colon", input: "web bin/rails server", want: "expected name: command"},
		{name: "missing command", input: "web:", want: "expected name: command"},
		{name: "duplicate", input: "web: one\nweb: two", want: "duplicate process"},
		{name: "empty", input: "\n# comment", want: "no processes found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}
