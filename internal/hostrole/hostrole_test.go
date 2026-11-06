package hostrole

import (
	"slices"
	"strings"
	"testing"
)

func TestParseReadsRolesInAnyOrderAndRefusesOthers(t *testing.T) {
	tests := []struct {
		text      string
		want      Set
		wantError string
	}{
		{"worker", Set{Worker}, ""},
		{"ingress,worker", Set{Worker, Ingress}, ""},
		{" ingress , control-plane ,worker", Set{ControlPlane, Worker, Ingress}, ""},
		{"", nil, "unknown role \"\""},
		{"worker,database", nil, "unknown role \"database\""},
		{"Worker", nil, "unknown role \"Worker\""},
		{"worker,ingress,worker", nil, "role worker named twice"},
		{"worker,", nil, "unknown role \"\""},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			roles, err := Parse(test.text)
			switch {
			case test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)):
				t.Errorf("want an error with %q, got %v", test.wantError, err)
			case test.wantError == "" && (err != nil || !slices.Equal(roles, test.want)):
				t.Errorf("got %v, %v; want %v", roles, err, test.want)
			}
		})
	}
}

func TestNewRefusesNoRoles(t *testing.T) {
	if roles, err := New(); err == nil || !strings.Contains(err.Error(), "no roles") {
		t.Errorf("got %v, %v", roles, err)
	}
}

func TestCheckTakesOnlySetsThatNewMakes(t *testing.T) {
	tests := []struct {
		name      string
		roles     Set
		wantError string
	}{
		{"every role", DefaultInit, ""},
		{"the join default", DefaultJoin, ""},
		{"empty", nil, "no roles"},
		{"unknown", Set{"database"}, "unknown role"},
		{"twice", Set{Worker, Worker}, "named twice"},
		{"out of order", Set{Ingress, ControlPlane}, "not in the order control-plane,worker,ingress"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.roles.Check()
			switch {
			case test.wantError == "" && err != nil:
				t.Errorf("want no error, got %v", err)
			case test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)):
				t.Errorf("want an error with %q, got %v", test.wantError, err)
			}
		})
	}
}

func TestWithinTellsWhetherEveryRoleIsAllowed(t *testing.T) {
	tests := []struct {
		name    string
		roles   Set
		allowed Set
		want    bool
	}{
		{"the same", DefaultJoin, DefaultJoin, true},
		{"fewer", Set{Worker}, DefaultJoin, true},
		{"one more", DefaultInit, DefaultJoin, false},
		{"another", Set{ControlPlane}, Set{Worker}, false},
		{"none allowed", Set{Worker}, nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.roles.Within(test.allowed); got != test.want {
				t.Errorf("%v within %v: got %v, want %v", test.roles, test.allowed, got, test.want)
			}
		})
	}
}

func TestStringIsWhatParseReads(t *testing.T) {
	for _, roles := range []Set{DefaultInit, DefaultJoin, {ControlPlane}, {ControlPlane, Ingress}} {
		parsed, err := Parse(roles.String())
		if err != nil || !slices.Equal(parsed, roles) {
			t.Errorf("%v: parsed %v, %v", roles, parsed, err)
		}
	}
	if text := DefaultInit.String(); text != "control-plane,worker,ingress" {
		t.Errorf("got %q", text)
	}
}

func TestHasTellsWhetherTheSetHoldsTheRole(t *testing.T) {
	if !DefaultJoin.Has(Worker) || DefaultJoin.Has(ControlPlane) {
		t.Errorf("%v: has worker %v, control-plane %v", DefaultJoin, DefaultJoin.Has(Worker), DefaultJoin.Has(ControlPlane))
	}
}
