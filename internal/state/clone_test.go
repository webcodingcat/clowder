package state

import (
	"fmt"
	"net/netip"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/webcodingcat/clowder/internal/resource"
)

// fill sets every exported field that it reaches to a value that is not
// zero: a map gets one entry, a slice one element, a pointer a value. So a
// field that Clone forgets shows in the tests below, whatever its type.
func fill(value reflect.Value) {
	switch value.Interface().(type) {
	case time.Time:
		value.Set(reflect.ValueOf(testTime))
		return
	case netip.Addr:
		value.Set(reflect.ValueOf(netip.MustParseAddr("10.0.0.2")))
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := range value.NumField() {
			if value.Type().Field(index).IsExported() {
				fill(value.Field(index))
			}
		}
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fill(value.Elem())
	case reflect.Map:
		key, element := reflect.New(value.Type().Key()).Elem(), reflect.New(value.Type().Elem()).Elem()
		fill(key)
		fill(element)
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(key, element)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		fill(value.Index(0))
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1)
	default:
		panic(fmt.Sprintf("fill: no value for %s", value.Type()))
	}
}

// sharedByRule are the parts that Clone shares on purpose, because the
// handlers only ever replace them whole: every spec, a stack's Services
// (worked out from its spec), every encrypted value and the certificates'
// bytes. Unexported fields belong to values such as netip.Addr, which
// never change in place, or are not part of the state (State.changes).
func sharedByRule(field reflect.StructField) bool {
	switch {
	case !field.IsExported():
		return true
	case field.Type == reflect.TypeFor[Encrypted]():
		return true
	}
	switch field.Name {
	case "Spec", "Chain", "CACertificate":
		return true
	case "Services":
		return field.Type == reflect.TypeFor[map[string]Service]()
	}
	return false
}

// shared lists the places where the clone and the original point to the
// same memory, apart from what sharedByRule allows.
func shared(path string, original, clone reflect.Value) []string {
	var found []string
	switch original.Kind() {
	case reflect.Struct:
		for index := range original.NumField() {
			field := original.Type().Field(index)
			if !sharedByRule(field) {
				found = append(found, shared(path+"."+field.Name, original.Field(index), clone.Field(index))...)
			}
		}
	case reflect.Pointer:
		if original.IsNil() || clone.IsNil() {
			return nil
		}
		if original.Pointer() == clone.Pointer() {
			return []string{path}
		}
		return shared(path, original.Elem(), clone.Elem())
	case reflect.Map:
		if original.Len() > 0 && original.Pointer() == clone.Pointer() {
			return []string{path}
		}
		for _, key := range original.MapKeys() {
			if copied := clone.MapIndex(key); copied.IsValid() {
				found = append(found, shared(fmt.Sprintf("%s[%v]", path, key), original.MapIndex(key), copied)...)
			}
		}
	case reflect.Slice:
		if original.Len() > 0 && clone.Len() > 0 && original.Pointer() == clone.Pointer() {
			return []string{path}
		}
		for index := range min(original.Len(), clone.Len()) {
			found = append(found, shared(fmt.Sprintf("%s[%d]", path, index), original.Index(index), clone.Index(index))...)
		}
	default:
	}
	return found
}

func TestACloneEqualsTheStateWithoutItsAudit(t *testing.T) {
	var full State
	fill(reflect.ValueOf(&full).Elem())
	clone := full.Clone()
	want := full
	want.Audit = nil
	if !reflect.DeepEqual(clone, &want) {
		t.Errorf("the clone differs from the state:\nclone %+v\nstate %+v", clone, &want)
	}
}

func TestACloneSharesOnlyWhatIsReplacedWhole(t *testing.T) {
	var full State
	fill(reflect.ValueOf(&full).Elem())
	clone := full.Clone()
	for _, path := range shared("State", reflect.ValueOf(full), reflect.ValueOf(*clone)) {
		t.Errorf("%s: the clone shares it with the state; copy it in Clone, or name it in sharedByRule "+
			"if no handler changes it in place", path)
	}
}

// bigSecretsCluster is a cluster that holds resource.MaxClusterSecretBytes
// in secrets: in as many namespaces as the limit of a namespace needs, in
// secrets of resource.MaxSecretBytes each. They come in one batch, because
// the tests encode the whole state at every command (cluster.applyAt).
func bigSecretsCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	perNamespace := resource.MaxNamespaceSecretBytes / resource.MaxSecretBytes
	var changes []ResourceChange
	for index := range resource.MaxClusterSecretBytes / resource.MaxSecretBytes {
		namespace := fmt.Sprintf("team%d", index/perNamespace)
		if index%perNamespace == 0 {
			changes = append(changes, putChange(resource.KindNamespace, "", namespace, nil))
		}
		changes = append(changes, putChange(resource.KindSecret, namespace, fmt.Sprintf("secret%d", index),
			encryptedValue(encryptedOf(resource.MaxSecretBytes), fmt.Sprintf("digest%d", index))))
	}
	c.mustApply(KindChangeResources, cliActor, batch(changes...))
	return c
}

// allocatedBytes returns how many bytes run allocates, on average over a
// few runs.
func allocatedBytes(run func()) uint64 {
	const runs = 10
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		run()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// TestAReportAndAnApplyCostLessThanTheSecrets shows what a state with
// resource.MaxClusterSecretBytes in secrets costs at a host's report (the
// command and the loops' shared copy after it) and at an apply of one
// small resource. Neither copies the secrets, so each stays far below
// their size.
func TestAReportAndAnApplyCostLessThanTheSecrets(t *testing.T) {
	c := bigSecretsCluster(t)
	report, err := NewCommand(KindReportHost, testTime, HostActor(0), ReportHost{Host: 0, Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	reported := allocatedBytes(func() {
		report.Payload = []byte(fmt.Sprintf(`{"host":0,"version":"1.0.%d"}`, c.index))
		c.index++
		if _, err := c.state.Apply(c.index, report); err != nil {
			t.Fatal(err)
		}
		_ = c.state.Clone() // the loops' shared copy (raftnode)
	})
	applied := allocatedBytes(func() {
		change := putChange(resource.KindNamespace, "", fmt.Sprintf("new%d", c.index), nil)
		apply, err := NewCommand(KindChangeResources, testTime, cliActor, batch(change))
		if err != nil {
			t.Fatal(err)
		}
		c.index++
		if _, err := c.state.Apply(c.index, apply); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("%d KiB of secrets in %d secrets: a report allocates %d KiB, an apply %d KiB",
		resource.MaxClusterSecretBytes>>10, len(c.state.Secrets), reported>>10, applied>>10)
	limit := uint64(resource.MaxClusterSecretBytes / 8)
	if reported > limit || applied > limit {
		t.Errorf("a report allocates %d bytes and an apply %d; each should stay below %d", reported, applied, limit)
	}
}
