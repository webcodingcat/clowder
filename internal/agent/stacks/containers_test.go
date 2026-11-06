package stacks

import (
	"testing"
)

func TestComposePsOutputIsRead(t *testing.T) {
	containers, err := parsePs(`{"Service":"app","State":"running","Health":"healthy","Status":"Up 1 minute"}` + "\n" +
		`{"Service":"db","State":"exited","Health":"","Status":"Exited (1)"}` + "\n")
	if err != nil || len(containers) != 2 || containers[0].Health != "healthy" || containers[1].State != "exited" {
		t.Errorf("%+v, %v", containers, err)
	}
	if _, err := parsePs("not json\n"); err == nil {
		t.Error("garbage was read")
	}
}
