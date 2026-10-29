// Package sopsage reads and writes SOPS files (YAML, age keys) without the
// SOPS library, which brings the SDKs of every cloud KMS with it. It follows
// the format of SOPS 3: values sealed with AES-256-GCM under a data key, the
// data key sealed with age for each recipient, and a MAC over all values.
// Files written here open with the sops tool and the other way round
// (bin/dev test-sops checks both directions against the real sops).
package sopsage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"
)

// metadataKey is the top-level key where SOPS keeps its metadata.
const metadataKey = "sops"

// defaultUnencryptedSuffix marks keys whose values SOPS leaves in plaintext.
const defaultUnencryptedSuffix = "_unencrypted"

// formatVersion goes into the files this package writes: the newest SOPS
// release whose format it follows. Older releases read such files too (the
// fixtures and test-sops use 3.12.2).
const formatVersion = "3.13.3"

// macOnlyEncryptedPrefix starts the MAC when mac_only_encrypted is set, as in
// SOPS, so such a MAC never equals one without the setting.
var macOnlyEncryptedPrefix = []byte{
	0x8a, 0x3f, 0xd2, 0xad, 0x54, 0xce, 0x66, 0x52, 0x7b, 0x10, 0x34, 0xf3, 0xd1, 0x47, 0xbe, 0x0b,
	0x0b, 0x97, 0x5b, 0x3b, 0xf4, 0x4f, 0x72, 0xc6, 0xfd, 0xad, 0xec, 0x81, 0x76, 0xf2, 0x7d, 0x69,
}

// metadata is the part of the sops block this package uses. Other keys
// (kms, pgp, ...) are allowed but not used.
type metadata struct {
	Age                     []ageStanza `yaml:"age"`
	LastModified            string      `yaml:"lastmodified"`
	MAC                     string      `yaml:"mac"`
	UnencryptedSuffix       string      `yaml:"unencrypted_suffix"`
	EncryptedSuffix         string      `yaml:"encrypted_suffix"`
	UnencryptedRegex        string      `yaml:"unencrypted_regex"`
	EncryptedRegex          string      `yaml:"encrypted_regex"`
	UnencryptedCommentRegex string      `yaml:"unencrypted_comment_regex"`
	EncryptedCommentRegex   string      `yaml:"encrypted_comment_regex"`
	MACOnlyEncrypted        bool        `yaml:"mac_only_encrypted"`
	KeyGroups               []any       `yaml:"key_groups"`
}

// ageStanza is the data key sealed for one age recipient.
type ageStanza struct {
	Recipient string `yaml:"recipient"`
	Encrypted string `yaml:"enc"`
}

// shouldBeEncrypted follows SOPS's rule for the suffix settings.
func (m metadata) shouldBeEncrypted(path []string) bool {
	if m.EncryptedSuffix != "" {
		for _, key := range path {
			if strings.HasSuffix(key, m.EncryptedSuffix) {
				return true
			}
		}
		return false
	}
	if m.UnencryptedSuffix != "" {
		for _, key := range path {
			if strings.HasSuffix(key, m.UnencryptedSuffix) {
				return false
			}
		}
	}
	return true
}

// Decrypt opens a SOPS file with one of the identities and returns the
// plaintext YAML without the sops block. The MAC must match.
func Decrypt(file []byte, identities []age.Identity) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(file, &document); err != nil {
		return nil, fmt.Errorf("read the file: %w", err)
	}
	root, meta, err := splitMetadata(&document)
	if err != nil {
		return nil, err
	}
	dataKey, err := openDataKey(meta, identities)
	if err != nil {
		return nil, err
	}
	hash := sha512.New()
	if meta.MACOnlyEncrypted {
		hash.Write(macOnlyEncryptedPrefix)
	}
	err = walkLeaves(root, nil, func(leaf *yaml.Node, path []string) error {
		encrypted := meta.shouldBeEncrypted(path)
		if !encrypted {
			plaintext, _, skip, err := plainValue(leaf)
			if err != nil || skip {
				return err
			}
			if !meta.MACOnlyEncrypted {
				hash.Write(plaintext)
			}
			return nil
		}
		if leaf.Value == "" || leaf.Tag == "!!null" {
			// SOPS leaves empty strings and nulls as they are; an empty
			// string adds nothing to the hash, and SOPS never hashes a null
			return nil
		}
		plaintext, valueType, err := decryptValue(leaf.Value, dataKey, strings.Join(path, ":")+":")
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Join(path, "."), err)
		}
		hash.Write(normalizeForMAC(plaintext, valueType))
		return setPlain(leaf, plaintext, valueType)
	})
	if err != nil {
		return nil, err
	}
	if err := checkMAC(meta, dataKey, hash.Sum(nil)); err != nil {
		return nil, err
	}
	// the plaintext is only decoded, never shown: YAML's usual indent
	return encodeYAML(&document, 2)
}

// normalizeForMAC: SOPS hashes the value after reading its type, so a bool
// written as "true" by another tool is hashed as "True".
func normalizeForMAC(plaintext []byte, valueType string) []byte {
	if valueType == "bool" {
		if strings.EqualFold(string(plaintext), "true") {
			return []byte("True")
		}
		return []byte("False")
	}
	return plaintext
}

// Encrypt seals plaintext YAML for the age recipients ("age1..." or SSH
// public keys) the way SOPS does, with the default settings.
func Encrypt(plaintext []byte, recipients []string, now time.Time) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("no recipients")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(plaintext, &document); err != nil {
		return nil, fmt.Errorf("read the plaintext: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the plaintext must be a YAML mapping")
	}
	root := document.Content[0]
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value == metadataKey {
			return nil, errors.New("the plaintext already has a sops block")
		}
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("make a data key: %w", err)
	}
	settings := metadata{UnencryptedSuffix: defaultUnencryptedSuffix}
	hash := sha512.New()
	err := walkLeaves(root, nil, func(leaf *yaml.Node, path []string) error {
		value, valueType, skip, err := plainValue(leaf)
		if err != nil || skip {
			return err
		}
		hash.Write(value)
		if !settings.shouldBeEncrypted(path) || (len(value) == 0 && valueType == "str") {
			return nil // SOPS leaves empty strings as they are
		}
		sealed, err := encryptValue(value, valueType, dataKey, strings.Join(path, ":")+":")
		if err != nil {
			return err
		}
		leaf.Tag, leaf.Value, leaf.Style = "!!str", sealed, 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	lastModified := now.UTC().Format(time.RFC3339)
	mac, err := encryptValue([]byte(fmt.Sprintf("%X", hash.Sum(nil))), "str", dataKey, lastModified)
	if err != nil {
		return nil, err
	}
	stanzas, err := sealDataKey(dataKey, recipients)
	if err != nil {
		return nil, err
	}
	root.Content = append(root.Content, metadataNode(stanzas, lastModified, mac)...)
	// the indent sops itself writes, so a re-encrypted file diffs cleanly
	return encodeYAML(&document, 4)
}

// splitMetadata takes the sops block out of the document and returns the
// data mapping and the metadata.
func splitMetadata(document *yaml.Node) (*yaml.Node, metadata, error) {
	var meta metadata
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, meta, errors.New("not a SOPS file: the top level is not a mapping")
	}
	root := document.Content[0]
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Value != metadataKey {
			continue
		}
		if err := root.Content[index+1].Decode(&meta); err != nil {
			return nil, meta, fmt.Errorf("read the sops block: %w", err)
		}
		root.Content = append(root.Content[:index], root.Content[index+2:]...)
		return root, meta, checkSupported(meta)
	}
	return nil, meta, errors.New("not a SOPS file: no sops block")
}

// checkSupported refuses settings this package does not implement, rather
// than decrypting the file wrongly.
func checkSupported(meta metadata) error {
	switch {
	case meta.UnencryptedRegex != "" || meta.EncryptedRegex != "" ||
		meta.UnencryptedCommentRegex != "" || meta.EncryptedCommentRegex != "":
		return errors.New("SOPS files with *_regex settings are not supported")
	case len(meta.KeyGroups) > 0:
		return errors.New("SOPS files with key groups are not supported")
	case len(meta.Age) == 0:
		return errors.New("the file has no age recipient (only age is supported)")
	}
	return nil
}

// openDataKey tries each age stanza with the identities.
func openDataKey(meta metadata, identities []age.Identity) ([]byte, error) {
	if len(identities) == 0 {
		return nil, errors.New("no age identity found (set SOPS_AGE_KEY_FILE or put the key where SOPS looks)")
	}
	for _, stanza := range meta.Age {
		reader, err := age.Decrypt(armor.NewReader(strings.NewReader(stanza.Encrypted)), identities...)
		var noMatch *age.NoIdentityMatchError
		switch {
		case errors.As(err, &noMatch):
			continue // sealed for someone else
		case err != nil:
			return nil, fmt.Errorf("open the data key sealed for %s: %w", stanza.Recipient, err)
		}
		dataKey, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read the data key: %w", err)
		}
		return dataKey, nil
	}
	return nil, errors.New("none of the age identities is a recipient of this file")
}

// checkMAC compares the MAC of the values with the one in the file.
func checkMAC(meta metadata, dataKey, sum []byte) error {
	lastModified, err := time.Parse(time.RFC3339, meta.LastModified)
	if err != nil {
		return fmt.Errorf("lastmodified %q: %w", meta.LastModified, err)
	}
	stored, _, err := decryptValue(meta.MAC, dataKey, lastModified.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("mac: %w", err)
	}
	if string(stored) != fmt.Sprintf("%X", sum) {
		return errors.New("the MAC does not match: the file was changed without SOPS")
	}
	return nil
}

// sealDataKey seals the data key for each recipient with age, ASCII-armored.
func sealDataKey(dataKey []byte, recipients []string) ([]ageStanza, error) {
	var stanzas []ageStanza
	for _, text := range recipients {
		recipient, err := parseRecipient(text)
		if err != nil {
			return nil, err
		}
		sealed, err := sealFor(dataKey, recipient)
		if err != nil {
			return nil, fmt.Errorf("seal the data key for %s: %w", text, err)
		}
		stanzas = append(stanzas, ageStanza{Recipient: text, Encrypted: sealed})
	}
	return stanzas, nil
}

// sealFor encrypts the data key for one recipient as an armored age file.
func sealFor(dataKey []byte, recipient age.Recipient) (string, error) {
	var sealed bytes.Buffer
	armored := armor.NewWriter(&sealed)
	writer, err := age.Encrypt(armored, recipient)
	if err != nil {
		return "", err
	}
	if _, err := writer.Write(dataKey); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	if err := armored.Close(); err != nil {
		return "", err
	}
	return sealed.String(), nil
}

// parseRecipient reads an age recipient or an SSH public key.
func parseRecipient(text string) (age.Recipient, error) {
	if strings.HasPrefix(text, "ssh-") {
		recipient, err := agessh.ParseRecipient(text)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", text, err)
		}
		return recipient, nil
	}
	recipient, err := age.ParseX25519Recipient(text)
	if err != nil {
		return nil, fmt.Errorf("recipient %q: %w", text, err)
	}
	return recipient, nil
}

// metadataNode builds the sops block as SOPS writes it with default settings.
func metadataNode(stanzas []ageStanza, lastModified, mac string) []*yaml.Node {
	scalar := func(value string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	}
	empty := func() *yaml.Node {
		return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	}
	ages := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, stanza := range stanzas {
		enc := scalar(stanza.Encrypted)
		enc.Style = yaml.LiteralStyle
		ages.Content = append(ages.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			scalar("recipient"), scalar(stanza.Recipient), scalar("enc"), enc,
		}})
	}
	block := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		scalar("kms"), empty(), scalar("gcp_kms"), empty(), scalar("azure_kv"), empty(),
		scalar("hc_vault"), empty(), scalar("age"), ages,
		scalar("lastmodified"), scalar(lastModified), scalar("mac"), scalar(mac),
		scalar("pgp"), empty(), scalar("unencrypted_suffix"), scalar(defaultUnencryptedSuffix),
		scalar("version"), scalar(formatVersion),
	}}
	return []*yaml.Node{scalar(metadataKey), block}
}

// encodeYAML writes a document with the given indent.
func encodeYAML(document *yaml.Node, indent int) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(indent)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("write YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("write YAML: %w", err)
	}
	return out.Bytes(), nil
}
