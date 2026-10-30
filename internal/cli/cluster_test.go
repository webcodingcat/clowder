package cli

import (
	"slices"
	"testing"
)

func TestDiffNamesTheFilesThatCameWentOrChanged(t *testing.T) {
	applied := map[string][]byte{"same.pem": []byte("a"), "changed.pem": []byte("a"), "gone.pem": []byte("a")}
	local := map[string][]byte{"same.pem": []byte("a"), "changed.pem": []byte("b"), "new.pem": []byte("a")}
	if got, want := changedFiles(applied, local), []string{"changed.pem", "gone.pem", "new.pem"}; !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
}
