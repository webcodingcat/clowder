package yamlpath

import "testing"

func TestPathsQuoteEveryKeyThatIsNoFieldName(t *testing.T) {
	for _, test := range []struct{ got, want string }{
		{Key("", "hoster"), "hoster"},
		{Key(Key("", "hoster"), "token"), "hoster.token"},
		{Key(Index(Key(Key("", "alerts"), "channels"), 0), "url"), "alerts.channels[0].url"},
		{Index(Key("", "join_keys"), 2), "join_keys[2]"},
		{Key(Key(Key("", "registries"), "ghcr.io"), "password"), `registries["ghcr.io"].password`},
		{Key(Key("", "registries"), "[2001:db8::1]:5000"), `registries["[2001:db8::1]:5000"]`},
		{Key(Key("", "registries"), "localhost"), "registries.localhost"},
		{Key("", "a\"b"), `["a\"b"]`},
		{Key("", ""), `[""]`},
	} {
		if test.got != test.want {
			t.Errorf("got %s, want %s", test.got, test.want)
		}
	}
}
