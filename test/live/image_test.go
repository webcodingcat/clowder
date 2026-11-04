//go:build live

package live

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The image the live tests' hosts start from (bin/dev live image): a server
// that setup.sh prepared (PREPARE_ONLY), cleaned and snapshotted.
const (
	imageServer = "clowder-image"
	// imageLabel carries the hash of the image's inputs (bin/dev: image_inputs)
	imageLabel     = "clowder-live-image"
	preparedMarker = "/var/lib/clowder-setup/prepared"
	// imageSettingsFile holds setup.sh's settings that bin/dev gives the
	// image (the package to read the dependencies from); imageIDFile is
	// where the test leaves the snapshot's ID for bin/dev to export
	imageSettingsFile = "image.env"
	imageIDFile       = "image-id"
	snapshotDeadline  = 15 * time.Minute
)

// cleanForReuse makes a prepared server's disk an image that every server
// starts from as from Debian's: cloud-init runs anew (a new instance, new
// SSH host keys, the network from the new metadata), systemd writes a new
// machine-id at the first boot, Docker makes a new engine ID, and nothing
// of the setup is left but what it installed. The package lists go too:
// they age, and setup.sh fetches them anew.
const cleanForReuse = `set -e
systemctl disable clowder-setup.service
rm -f /etc/systemd/system/clowder-setup.service /usr/local/sbin/clowder-setup /etc/clowder-setup.env
rm -rf /var/lib/clowder-setup
apt-get clean
rm -rf /var/lib/apt/lists/*
rm -f /var/lib/docker/engine-id
rm -f /etc/ssh/ssh_host_*
cloud-init clean --logs --machine-id
journalctl --rotate --vacuum-time=1s
sync`

// TestImage makes the image of the live tests' hosts: a server of Debian 13
// runs setup.sh with PREPARE_ONLY, is cleaned for reuse and snapshotted.
// bin/dev live image runs it in a lab of its own and exports the snapshot;
// the scenarios do not.
func TestImage(t *testing.T) {
	label := os.Getenv("IMAGE_LABEL")
	if label == "" {
		t.Fatal("set IMAGE_LABEL to the hash of the image's inputs; bin/dev live image does")
	}
	var serverID int64
	var server host
	runSteps(t, []step{
		{"a server prepares the image", func(t *testing.T) { serverID, server = prepareServer(t) }},
		{"the server is cleaned and powered off", func(t *testing.T) {
			server.must(t, cleanForReuse)
			// the answer may not come back before the connection goes
			_, _ = server.run("systemctl --no-block poweroff")
			eventually(t, 5*time.Minute, "the image's server off", func() error {
				if status := serverStatusByID(t, serverID); status != "off" {
					return fmt.Errorf("it is %s", status)
				}
				return nil
			})
		}},
		{"the snapshot is made", func(t *testing.T) { snapshot(t, serverID, label) }},
	})
}

// prepareServer makes a server whose cloud-init runs setup.sh with
// PREPARE_ONLY, and waits until the setup is done.
func prepareServer(t *testing.T) (int64, host) {
	publicKey, err := os.ReadFile(filepath.Join(workDir, "id_ed25519.pub"))
	if err != nil {
		t.Fatal(err)
	}
	var key struct {
		SSHKey struct {
			ID int64 `json:"id"`
		} `json:"ssh_key"`
	}
	cloud(t, http.MethodPost, "/ssh_keys", map[string]any{"name": imageServer, "public_key": string(publicKey)}, &key)
	var created struct {
		Server struct {
			ID        int64 `json:"id"`
			PublicNet struct {
				IPv4 struct {
					IP string `json:"ip"`
				} `json:"ipv4"`
			} `json:"public_net"`
		} `json:"server"`
	}
	cloud(t, http.MethodPost, "/servers", map[string]any{
		// the module's defaults: a host's disk must hold the snapshot
		"name": imageServer, "server_type": "cx23", "image": "debian-13", "location": "nbg1",
		"ssh_keys": []int64{key.SSHKey.ID}, "user_data": imageUserData(t),
	}, &created)
	server := host{address: created.Server.PublicNet.IPv4.IP}
	eventually(t, setupDeadline, "the image prepared", server.setupReached(preparedMarker))
	return created.Server.ID, server
}

// imageUserData is cloud-init's user data of the image's server: setup.sh
// and its unit as examples/hetzner writes them, with the settings that
// bin/dev gave and PREPARE_ONLY.
func imageUserData(t *testing.T) string {
	t.Helper()
	read := func(name string) string {
		content, err := os.ReadFile(filepath.Join(sourceDir(), "examples", "hetzner", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	settings, err := os.ReadFile(filepath.Join(workDir, imageSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	type file struct {
		Path        string `yaml:"path"`
		Permissions string `yaml:"permissions"`
		Content     string `yaml:"content"`
	}
	config, err := yaml.Marshal(map[string]any{
		"write_files": []file{
			{"/usr/local/sbin/clowder-setup", "0755", read("setup.sh")},
			{"/etc/systemd/system/clowder-setup.service", "0644", read("clowder-setup.service")},
			{"/etc/clowder-setup.env", "0600", string(settings) + "PREPARE_ONLY='yes'\n"},
		},
		"runcmd": [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "enable", "--no-block", "--now", "clowder-setup.service"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "#cloud-config\n" + string(config)
}

// serverStatusByID is the status of a server at the hoster.
func serverStatusByID(t *testing.T, id int64) string {
	t.Helper()
	var answer struct {
		Server struct {
			Status string `json:"status"`
		} `json:"server"`
	}
	cloud(t, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &answer)
	return answer.Server.Status
}

// snapshot makes the snapshot of the image's server with its label, waits
// until it is available and writes its ID for bin/dev. The server stays:
// bin/dev removes the whole lab.
func snapshot(t *testing.T, serverID int64, label string) {
	var created struct {
		Image struct {
			ID int64 `json:"id"`
		} `json:"image"`
	}
	cloud(t, http.MethodPost, fmt.Sprintf("/servers/%d/actions/create_image", serverID), map[string]any{
		"type": "snapshot", "description": "clowder live host", "labels": map[string]string{imageLabel: label},
	}, &created)
	imageID := strconv.FormatInt(created.Image.ID, 10)
	eventually(t, snapshotDeadline, "the snapshot available", func() error {
		var answer struct {
			Image struct {
				Status string `json:"status"`
			} `json:"image"`
		}
		cloud(t, http.MethodGet, "/images/"+imageID, nil, &answer)
		if answer.Image.Status != "available" {
			return fmt.Errorf("it is %s", answer.Image.Status)
		}
		return nil
	})
	if err := os.WriteFile(filepath.Join(workDir, imageIDFile), []byte(imageID), 0o600); err != nil {
		t.Fatal(err)
	}
}
