package secretvalue

import (
	"bytes"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
)

func identities(t *testing.T) (cluster, recovery *age.X25519Identity) {
	t.Helper()
	cluster, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err = age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return cluster, recovery
}

// armorOf encrypts plaintext as it stands, without an envelope.
func armorOf(t *testing.T, plaintext string, recipient age.Recipient) string {
	t.Helper()
	var armored bytes.Buffer
	armorWriter := armor.NewWriter(&armored)
	writer, err := age.Encrypt(armorWriter, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := armorWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return armored.String()
}

func TestEitherKeyOpensWhatIsEncryptedForBoth(t *testing.T) {
	cluster, recovery := identities(t)
	value := Value{For: "shop/db-password", Bytes: []byte("s3cret\x00\xff<&>")}
	armored, err := Encrypt(value, cluster.Recipient(), recovery.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(armored, armor.Header+"\n") || !strings.HasSuffix(armored, armor.Footer+"\n") {
		t.Errorf("not age's armor with one line end:\n%s", armored)
	}
	for _, identity := range []age.Identity{cluster, recovery} {
		opened, err := Open(armored, identity)
		if err != nil {
			t.Fatal(err)
		}
		if opened.For != value.For || !bytes.Equal(opened.Bytes, value.Bytes) {
			t.Errorf("opened %q for %s", opened.Bytes, opened.For)
		}
	}
	other, _ := identities(t)
	if _, err := Open(armored, other); err == nil {
		t.Error("another key opened the value")
	}
}

func TestOpenRefusesWhatClowderDidNotWriteWithoutTheValue(t *testing.T) {
	cluster, _ := identities(t)
	for _, test := range []struct{ plaintext, want string }{
		{"hunter2-in-clear", "no envelope"},
		{`{"v":1,"for":"","value":"aHVudGVy"}`, "no envelope"},
		{`{"value":"aHVudGVy","for":"cluster"}`, "no envelope"},
		{`{"v":2,"for":"cluster","value":"aHVudGVy","new":true}`, "newer clowder"},
		{`{"v":1,"for":"cluster","value":"not base64 hunter2"}`, "no envelope"},
	} {
		_, err := Open(armorOf(t, test.plaintext, cluster.Recipient()), cluster)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want %q", test.plaintext, err, test.want)
			continue
		}
		if strings.Contains(err.Error(), "hunter") || strings.Contains(err.Error(), "aHVudGVy") {
			t.Errorf("the error names the value: %v", err)
		}
	}
	if _, err := Open("no armor at all", cluster); err == nil {
		t.Error("opened text that is no armor")
	}
}

func TestEncryptNeedsAPlace(t *testing.T) {
	cluster, _ := identities(t)
	if _, err := Encrypt(Value{Bytes: []byte("x")}, cluster.Recipient()); err == nil {
		t.Error("encrypted a value for no place")
	}
}

// The request size counts a value as 1.84 times its size in a request (two
// rounds of Base64, armor's lines, JSON's \n) and a fixed part of less than
// 1 KiB, with the resource's JSON.
func TestTheArmorGrowsAsTheRequestSizeCounts(t *testing.T) {
	cluster, recovery := identities(t)
	longest := strings.Repeat("n", 63) + "/" + strings.Repeat("s", 63)
	for _, size := range []int{0, 32, 64 << 10} {
		armored, err := Encrypt(Value{For: longest, Bytes: bytes.Repeat([]byte{0xff}, size)},
			cluster.Recipient(), recovery.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		// a JSON request writes each line end as \n
		inRequest := len(armored) + strings.Count(armored, "\n")
		if budget := size*184/100 + 800; inRequest > budget {
			t.Errorf("%d bytes take %d in a request, more than %d", size, inRequest, budget)
		}
	}
}
