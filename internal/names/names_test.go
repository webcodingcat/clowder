package names

import "testing"

func TestStackAndSecretNamesAreWhatComposeAllows(t *testing.T) {
	for name, valid := range map[string]bool{
		"shop": true, "shop-2": true, "shop_db": true, "0day": true,
		"": false, "Shop": false, "-shop": false, "shop/db": false, "shop.db": false, "..": false,
	} {
		if ValidStack(name) != valid || ValidSecret(name) != valid {
			t.Errorf("%q: want valid %v", name, valid)
		}
	}
}

func TestClowdersOwnProjectNamesAreNoStacks(t *testing.T) {
	if ValidStack("clowder-traefik") || !ValidStack("clowder") || !ValidStack("my-clowder-app") {
		t.Error("only names that start with clowder- are reserved")
	}
	if !ValidSecret("clowder-token") {
		t.Error("a secret may start with clowder-")
	}
}

func TestVariableNames(t *testing.T) {
	for name, valid := range map[string]bool{
		"DB_PASSWORD": true, "_x": true, "a1": true, "": false, "1A": false, "A-B": false, "A=B": false,
	} {
		if ValidVariable(name) != valid {
			t.Errorf("%q: want valid %v", name, valid)
		}
	}
}
