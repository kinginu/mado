package render

import (
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	in := "Results:\n\n| op | us |\n|----|---:|\n| add | 14.5 |\n| ~~mul~~ | 3 |\n\n" +
		"```go\nfunc main() { fmt.Println(\"<b>\") }\n```\n\n" +
		"Inline <script>alert(1)</script> attempt and a block:\n\n<div onclick=\"x()\">raw</div>\n\n" +
		"See [docs](https://example.com/a?b=1&c=2), https://go.dev and [bad](javascript:alert(1)).\n\n" +
		"- [x] done\n- [ ] todo\n"
	out := string(Markdown(in))
	wants := []string{
		`<div class="table-wrap"><table>`,
		`<th>op</th>`,
		`<td align="right">14.5</td>`,
		`<del>mul</del>`,
		`<pre><code class="language-go">func main() { fmt.Println(&quot;&lt;b&gt;&quot;) }`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`,
		`&lt;div onclick=&#34;x()&#34;&gt;raw&lt;/div&gt;`,
		`<a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer" target="_blank">docs</a>`,
		`<a href="https://go.dev" rel="noopener noreferrer" target="_blank">https://go.dev</a>`,
		`type="checkbox"`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	for _, bad := range []string{"style=", "<script", "<div onclick", "javascript:", "raw HTML omitted"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
}

// The recorded assistant text renders as one paragraph; the tty stays plain text.
func TestMarkdownRecordedFixture(t *testing.T) {
	var got []string
	for _, l := range loadFixture(t, "write_tool.jsonl") {
		for _, b := range Blocks(l) {
			if b.Kind == KindAssistant || b.Kind == KindUser {
				got = append(got, string(Markdown(b.Text)))
			}
		}
	}
	if len(got) != 1 || strings.TrimSpace(got[0]) != "<p>done</p>" {
		t.Fatalf("markdown of fixture text = %q", got)
	}
	tty := TTY([]Block{{Kind: KindAssistant, Text: "| a |\n|---|\n| **b** |"}})
	if !strings.Contains(tty, "| **b** |") || strings.Contains(tty, "<") {
		t.Errorf("tty is not plain text: %q", tty)
	}
}
