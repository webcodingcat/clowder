package live

import (
	"bufio"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// notScenarios are the tests of the package that bin/dev live runs apart
// from the scenarios: TestSetup makes the cluster (live up), TestImage the
// hosts' image (live image).
var notScenarios = []string{"TestSetup", "TestImage"}

// TestEveryScenarioIsListedOnce checks the file scenarios against the
// scenarios of the package: bin/dev live runs only what the file lists, so a
// scenario left out would never run. It runs with the unit tests, without
// the build tag live.
func TestEveryScenarioIsListedOnce(t *testing.T) {
	listed := listedScenarios(t)
	for _, name := range liveTests(t) {
		if slices.Contains(notScenarios, name) {
			continue
		}
		switch count := listed[name]; count {
		case 1:
		case 0:
			t.Errorf("%s is missing in the file scenarios", name)
		default:
			t.Errorf("%s is %d times in the file scenarios", name, count)
		}
		delete(listed, name)
	}
	for name := range listed {
		t.Errorf("the file scenarios lists %s, which is no scenario of the package", name)
	}
}

// listedScenarios reads the file scenarios: how often it names each test.
// Each line is a name and a run time in minutes; # starts a comment line.
func listedScenarios(t *testing.T) map[string]int {
	t.Helper()
	file, err := os.Open("scenarios")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }() // read only
	listed := map[string]int{}
	lines := bufio.NewScanner(file)
	for number := 1; lines.Scan(); number++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Errorf("scenarios:%d: want a name and minutes, got %q", number, line)
			continue
		}
		if minutes, err := strconv.ParseFloat(fields[1], 64); err != nil || minutes <= 0 {
			t.Errorf("scenarios:%d: %q are no minutes", number, fields[1])
		}
		listed[fields[0]]++
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return listed
}

// liveTests lists the tests in the package's files with the build tag
// live: go test runs each func TestXxx(t *testing.T) there.
func liveTests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fileSet := token.NewFileSet()
	for _, name := range files {
		parsed, err := parser.ParseFile(fileSet, name, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if !hasLiveTag(parsed) {
			continue
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") &&
				function.Type.Params.NumFields() == 1 && function.Name.Name != "TestMain" {
				names = append(names, function.Name.Name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("found no test with the build tag live")
	}
	return names
}

// hasLiveTag tells whether a file builds only with the tag live.
func hasLiveTag(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			// false with every tag set but live: the file needs live
			expression, err := constraint.Parse(comment.Text)
			if err == nil && !expression.Eval(func(tag string) bool { return tag != "live" }) {
				return true
			}
		}
	}
	return false
}
