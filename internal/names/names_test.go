package names

import "testing"

func TestStackNamesAreWhatComposeAllows(t *testing.T) {
	for name, valid := range map[string]bool{
		"shop": true, "shop-2": true, "shop_db": true, "0day": true,
		"": false, "Shop": false, "-shop": false, "shop/db": false, "shop.db": false,
	} {
		if ValidStack(name) != valid {
			t.Errorf("%q: valid %v, want %v", name, !valid, valid)
		}
	}
}
