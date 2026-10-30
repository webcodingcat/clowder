package metrics

import (
	"strings"
	"testing"
)

func TestTheTextFormatIsSortedAndEscaped(t *testing.T) {
	set := NewSet()
	set.Add("b_total", "Second.", Counter, 3, "name", `a "quoted"\ value`)
	set.Add("a_bytes", "First,\nwith a line feed.", Gauge, 1<<30, "host", "node1")
	set.Add("a_bytes", "ignored: the first sample's help counts", Gauge, 0.5, "host", "node0")
	var out strings.Builder
	if err := set.Write(&out); err != nil {
		t.Fatal(err)
	}
	want := "# HELP a_bytes First,\\nwith a line feed.\n# TYPE a_bytes gauge\n" +
		"a_bytes{host=\"node0\"} 0.5\na_bytes{host=\"node1\"} 1073741824\n" +
		"# HELP b_total Second.\n# TYPE b_total counter\n" +
		"b_total{name=\"a \\\"quoted\\\"\\\\ value\"} 3\n"
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestLabelsComeInPairs(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an odd list of labels was taken")
		}
	}()
	// a slice the linter cannot see through: it refuses an odd literal list
	odd := strings.Fields("host")
	NewSet().Add("a", "A.", Gauge, 1, odd...)
}
