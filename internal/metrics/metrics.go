// Package metrics writes Prometheus' text format by hand: clowderd exports
// a few dozen values, too few for Prometheus' client library and what it
// brings along. Families and samples come out sorted, so the output is the
// same from scrape to scrape and can be tested against a golden file.
package metrics

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Kind is a family's type in the text format.
type Kind string

// The kinds clowder uses.
const (
	Gauge   Kind = "gauge"
	Counter Kind = "counter"
)

// family is one metric with its samples.
type family struct {
	help    string
	kind    Kind
	samples []sample
}

// sample is one value of a family, told apart by its labels.
type sample struct {
	labels map[string]string
	value  float64
}

// Set collects the families of one scrape.
type Set struct {
	families map[string]*family
}

// NewSet is an empty set.
func NewSet() *Set {
	return &Set{families: map[string]*family{}}
}

// Add adds a sample to a family, which the first sample defines; labels are
// names and values in turn. An odd list is a bug of the caller, so it
// panics: the tests catch it.
func (s *Set) Add(name, help string, kind Kind, value float64, labels ...string) {
	if len(labels)%2 != 0 {
		panic(fmt.Sprintf("metric %s: labels %q are not pairs of name and value", name, labels))
	}
	metric, ok := s.families[name]
	if !ok {
		metric = &family{help: help, kind: kind}
		s.families[name] = metric
	}
	added := sample{labels: map[string]string{}, value: value}
	for index := 0; index < len(labels); index += 2 {
		added.labels[labels[index]] = labels[index+1]
	}
	metric.samples = append(metric.samples, added)
}

// Write writes the set in the text format, families by name and samples by
// their labels.
func (s *Set) Write(out io.Writer) error {
	var text strings.Builder
	for _, name := range slices.Sorted(maps.Keys(s.families)) {
		metric := s.families[name]
		fmt.Fprintf(&text, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(metric.help), name, metric.kind)
		lines := make([]string, 0, len(metric.samples))
		for _, each := range metric.samples {
			lines = append(lines, name+labels(each.labels)+" "+formatValue(each.value)+"\n")
		}
		slices.SortFunc(lines, cmp.Compare)
		for _, line := range lines {
			text.WriteString(line)
		}
	}
	_, err := io.WriteString(out, text.String())
	return err
}

// labels writes {name="value",...} sorted by name; nothing for none.
func labels(set map[string]string) string {
	if len(set) == 0 {
		return ""
	}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(set)) {
		parts = append(parts, name+`="`+escapeValue(set[name])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeValue escapes a label value as the text format wants: backslash,
// double quote and line feed.
func escapeValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}

// escapeHelp escapes a help text: backslash and line feed.
func escapeHelp(help string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help)
}

// formatValue writes a value without an exponent for whole numbers, so
// byte counts read as they are.
func formatValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
