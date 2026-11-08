package policy

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		in     string
		cmds   [][]string
		opaque bool
	}{
		{`echo "a && b"`, [][]string{{"echo", "a && b"}}, false},
		{`go test ./... && git status`, [][]string{{"go", "test", "./..."}, {"git", "status"}}, false},
		{"a; b || c | d |& e & f\ng", [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}, {"f"}, {"g"}}, false},
		{`echo 'x;y' "p|q" r\;s`, [][]string{{"echo", "x;y", "p|q", "r;s"}}, false},
		{`echo $(rm -rf /)`, [][]string{{"rm", "-rf", "/"}, {"echo", ""}}, true},
		{"echo `id`", [][]string{{"id"}, {"echo", ""}}, true},
		{`echo "pre $(whoami) post"`, [][]string{{"whoami"}, {"echo", "pre  post"}}, true},
		{`(cd /x && ls)`, [][]string{{"cd", "/x"}, {"ls"}}, true},
		{`diff <(ls a) b`, [][]string{{"ls", "a"}, {"diff", "b"}}, true},
		{`ls # a comment; rm -rf /`, [][]string{{"ls"}}, false},
		{`echo "unterminated`, [][]string{{"echo", "unterminated"}}, true},
		{`{ ls; }`, [][]string{{"ls"}}, true},
		{"cat > f.txt <<'EOF'\nsudo rm -rf / ; kill -9 1\nEOF\nls", [][]string{{"cat"}, {"ls"}}, false},
		{"cat <<EOF\n$(id)\nEOF", [][]string{{"cat"}}, true},
		{"cat <<EOF", [][]string{{"cat"}}, true},
		{"echo a \\\n b", [][]string{{"echo", "a", "b"}}, false},
	}
	for _, c := range cases {
		p := Split(c.in)
		var got [][]string
		for _, cm := range p.Commands {
			got = append(got, cm.Texts())
		}
		if !reflect.DeepEqual(got, c.cmds) || (p.Opaque != "") != c.opaque {
			t.Errorf("Split(%q) = %q opaque=%q, want %q opaque=%v", c.in, got, p.Opaque, c.cmds, c.opaque)
		}
	}
}

func TestSplitRedirections(t *testing.T) {
	p := Split(`go test ./... 2>&1 >out.txt >> /tmp/log < in 2>/dev/null &> all`)
	if len(p.Commands) != 1 || p.Opaque != "" {
		t.Fatalf("%+v", p)
	}
	var got []string
	for _, r := range p.Commands[0].Redirs {
		got = append(got, r.Op+" "+r.Target.Text)
	}
	want := []string{">& &1", "> out.txt", ">> /tmp/log", "< in", "> /dev/null", "&> all"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("redirs %q, want %q", got, want)
	}
	if ws := p.Commands[0].Texts(); !reflect.DeepEqual(ws, []string{"go", "test", "./..."}) {
		t.Errorf("words %q", ws)
	}
	w := Split(`cat ~/x "$HOME" '$HOME'`).Commands[0].Words
	if !w[1].Tilde || !w[2].Var || w[3].Var || w[3].Tilde {
		t.Errorf("word flags %+v", w)
	}
}
