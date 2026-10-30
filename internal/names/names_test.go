package names

import (
	"strings"
	"testing"
)

func TestNamesAreShortDNSLabels(t *testing.T) {
	for resourceName, valid := range map[string]bool{
		"shop": true, "shop-2": true, "0day": true, "a": true, strings.Repeat("a", MaxLength): true,
		"": false, "Shop": false, "-shop": false, "shop-": false, "shop_db": false, "shop/db": false,
		"shop.db": false, "..": false, strings.Repeat("a", MaxLength+1): false,
	} {
		if Valid(resourceName) != valid {
			t.Errorf("%q: want valid %v", resourceName, valid)
		}
	}
}

func TestClowdersOwnProjectsAreNoNamespaces(t *testing.T) {
	if ValidNamespace("clowder-traefik") || !ValidNamespace("clowder") || !ValidNamespace("my-clowder-app") {
		t.Error("only namespaces that start with clowder- are reserved")
	}
	if !Valid("clowder-traefik") {
		t.Error("a stack may start with clowder-: its project starts with its namespace")
	}
	if strings.HasPrefix(Project(Default, "clowder-traefik"), ReservedPrefix) {
		t.Error("a stack's project must never look like clowder's own")
	}
}

func TestIDsSplitBack(t *testing.T) {
	namespace, stack, ok := SplitID(ID("shop", "web"))
	if !ok || namespace != "shop" || stack != "web" {
		t.Errorf("SplitID(ID(shop, web)) = %q, %q, %v", namespace, stack, ok)
	}
	for _, id := range []string{"shop", "shop/", "/web", "shop/web/data", "clowder-x/web", "Shop/web"} {
		if _, _, ok := SplitID(id); ok {
			t.Errorf("SplitID(%q) is ok", id)
		}
	}
}

func TestProjectsAndFilesOfTwoNamespacesNeverMeet(t *testing.T) {
	if Project("a-b", "c") == Project("a", "b-c") {
		t.Error("the projects of a-b/c and a/b-c are the same")
	}
	if FileName(VolumeID("a-b", "c", "d")) == FileName(VolumeID("a", "b-c", "d")) {
		t.Error("the volumes of a-b/c and a/b-c share their file")
	}
}

func TestAVolumeIDTellsItsStack(t *testing.T) {
	stack, ok := StackOfVolume(VolumeID("shop", "web", "data"))
	if !ok || stack != "shop/web" {
		t.Errorf("StackOfVolume = %q, %v", stack, ok)
	}
	for _, id := range []string{"shop/data", "shop/web/", "data", "Shop/web/data", "shop/web/da_ta"} {
		if _, ok := StackOfVolume(id); ok {
			t.Errorf("StackOfVolume(%q) is ok", id)
		}
	}
}

func TestVolumeNamesOfDifferentVolumesNeverMeet(t *testing.T) {
	if VolumeName(VolumeID("a-b", "c", "d"), 1) == VolumeName(VolumeID("a", "b-c", "d"), 1) {
		t.Error("the hash must keep a-b/c and a/b-c apart")
	}
	if VolumeName("shop/web/db", 1) == VolumeName("shop/web/db", 2) {
		t.Error("generations must differ")
	}
	long := strings.Repeat("a", MaxLength)
	if VolumeName(VolumeID(long, long, "x"), 1) == VolumeName(VolumeID(long, long, "y"), 1) {
		t.Error("the hash must keep volumes apart whose readable part is cut alike")
	}
}

// TestVolumeNamesFitDeviceMapper checks the worst case of the limits that
// VolumeName's comment names: names full of "-", the longest volume group
// full of "-" and the longest generation.
func TestVolumeNamesFitDeviceMapper(t *testing.T) {
	dashes := "a" + strings.Repeat("-", MaxLength-2) + "a"
	if !Valid(dashes) {
		t.Fatalf("%q is no valid name", dashes)
	}
	lv := VolumeName(VolumeID(dashes, dashes, dashes), MaxDataGeneration)
	if len(lv) > 127 {
		t.Errorf("LV %s has %d characters, more than LVM and DRBD take", lv, len(lv))
	}
	group := "v" + strings.Repeat("-", MaxVolumeGroupLength-1)
	deviceMapper := strings.ReplaceAll(group, "-", "--") + "-" + strings.ReplaceAll(lv, "-", "--")
	if len(deviceMapper) > 127 {
		t.Errorf("device-mapper name %s has %d characters, more than 127", deviceMapper, len(deviceMapper))
	}
}

func TestBridgeNamesFitTheKernelLimit(t *testing.T) {
	long := strings.Repeat("a", MaxLength)
	if name := BridgeName(ID(long, long), "backend"); len(name) > 15 {
		t.Errorf("%s has %d characters, more than an interface name may have", name, len(name))
	}
	if BridgeName("shop/web", "default") == BridgeName("shop/web", "backend") ||
		BridgeName("a/web", "default") == BridgeName("b/web", "default") {
		t.Error("bridges of different networks must differ")
	}
}

func TestDomainsHaveTwoLabelsAtLeast(t *testing.T) {
	for domainName, valid := range map[string]bool{
		"shop.example.com": true, "Shop.Example.COM": true, "a-b.de": true, "x.y": true,
		"": false, "localhost": false, "-a.de": false, "a-.de": false, "a..de": false, "a.de.": false,
		"*.example.com": false, "a_b.de": false, "a.de`": false,
	} {
		if ValidDomain(domainName) != valid {
			t.Errorf("%q: want valid %v", domainName, valid)
		}
	}
}

func TestVariableNames(t *testing.T) {
	for variableName, valid := range map[string]bool{
		"DB_PASSWORD": true, "_x": true, "a1": true, "": false, "1A": false, "A-B": false, "A=B": false,
	} {
		if ValidVariable(variableName) != valid {
			t.Errorf("%q: want valid %v", variableName, valid)
		}
	}
}
