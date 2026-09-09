package mentions

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	known := map[string]bool{"alice": true, "bob": true, "bob-2": true}

	cases := []struct {
		body      string
		want      []string
		broadcast bool
	}{
		{"hey @alice and @bob", []string{"alice", "bob"}, false},
		{"@alice @alice dedup", []string{"alice"}, false},
		{"email me a@alice.com", nil, false},
		{"@unknown ping", nil, false},
		{"@channel deploy done", nil, true},
		{"@here and @bob-2", []string{"bob-2"}, true},
		{"describe `@channel` without notifying", nil, false},
		{"code `@alice` without notifying", nil, false},
		{"```\n@here in a fence\n```", nil, false},
		{"(@alice)", []string{"alice"}, false},
		{"no mentions", nil, false},
	}
	for _, c := range cases {
		got, b := Parse(c.body, known)
		if !reflect.DeepEqual(got, c.want) || b != c.broadcast {
			t.Errorf("Parse(%q) = %v,%v want %v,%v", c.body, got, b, c.want, c.broadcast)
		}
	}
}

func TestParseSpacedAndUpperNames(t *testing.T) {
	known := map[string]bool{"John": true, "John Smith": true, "Data Bot": true}

	cases := []struct {
		body      string
		want      []string
		broadcast bool
	}{
		{"ping @John Smith please", []string{"John Smith"}, false},
		{"ping @John, thanks", []string{"John"}, false},
		{"@Data Bot run it", []string{"Data Bot"}, false},
		{"@john lowercase is a different name", nil, false},
		{"@John Smith and @John too", []string{"John Smith", "John"}, false},
		{"@CHANNEL loud", nil, true},
	}
	for _, c := range cases {
		got, b := Parse(c.body, known)
		if !reflect.DeepEqual(got, c.want) || b != c.broadcast {
			t.Errorf("Parse(%q) = %v,%v want %v,%v", c.body, got, b, c.want, c.broadcast)
		}
	}
}

func TestUnknown(t *testing.T) {
	known := map[string]bool{"John": true, "John Smith": true, "Data Bot": true}
	cases := []struct {
		body string
		want []string
	}{
		{"ping @John please", nil},
		{"ping @John Smith please", nil},
		{"ping @ghost please", []string{"ghost"}},
		{"@ghost and @ghost again", []string{"ghost"}},
		{"@ghost then @phantom", []string{"ghost", "phantom"}},
		{"mail me at foo@example.com", nil},
		{"@channel @here @everyone", nil},
		{"code: `@ghost` stays quiet", nil},
		{"```\n@ghost in a fence\n```", nil},
		{"@Data Bot is fine but @Data Robot is not", []string{"Data"}},
		{"emails like a@b.co and handles like @nobody", []string{"nobody"}},
	}
	for _, c := range cases {
		if got := Unknown(c.body, known); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Unknown(%q) = %v want %v", c.body, got, c.want)
		}
	}
}
