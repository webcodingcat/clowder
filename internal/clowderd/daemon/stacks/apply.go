package stacks

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
)

// apply leaves alone the LVs in inUse: a stack that failed to stop may
// still write to them.
func (r *Runner) apply(ctx context.Context, desired desiredState, stack *state.Stack, inUse volumesInUse) error {
	secrets, err := r.openSecrets(ctx, desired, stack)
	if err != nil {
		return err
	}
	volumes, err := r.stackVolumes(desired, stack, inUse)
	if err != nil {
		return err
	}
	rendered, err := r.render(desired, stack, volumes, secrets)
	if err != nil {
		return err
	}
	config, missing, err := r.registries.Write(ctx, desired.copied, stack.Namespace)
	if err != nil {
		return err
	}
	if err := r.up(ctx, config, stack, rendered, secrets, volumes); err != nil {
		// like the kubelet, the runner goes on without the missing image
		// pull secret. If the pull fails, get stacks says why.
		if missing != nil {
			return fmt.Errorf("%w; %w", missing, err) // the first line is what get stacks shows
		}
		return err
	}
	return r.reportRunning(ctx, stack, secrets)
}

var errVolumeNotReady = errors.New("a volume is not mounted on this host yet")

// stackVolumes may be outdated by the time compose up runs. The reconciler
// may read a newer state in which the stack has left, and unmount a copy
// before writeStackFiles lists the LVs as held. Then compose up fails on
// the missing data/ (the mount guard), and the next pass stops the stack.
func (r *Runner) stackVolumes(desired desiredState, stack *state.Stack, inUse volumesInUse) (
	map[string]compose.MountedVolume, error,
) {
	volumes := map[string]compose.MountedVolume{}
	for _, name := range stack.Spec.VolumeNames() {
		id := names.ID(stack.Namespace, name)
		volume, ok := desired.volumes[id]
		// the placement commits a stack's volumes before its host, so a
		// missing copy is a bug in the placement, not something to wait for
		if !ok || !slices.ContainsFunc(volume.Copies, func(c state.VolumeCopy) bool { return c.Host == r.host }) {
			return nil, fmt.Errorf("volume %s has no copy on this host", name)
		}
		lv := names.VolumeName(id, volume.DataGeneration)
		if user, ok := inUse.user(lv); ok {
			return nil, fmt.Errorf("%w: volume %s, by %s", errVolumeInUse, name, user)
		}
		dir, ready, err := r.volumes.Mounted(lv)
		switch {
		case err != nil:
			return nil, err
		case !ready:
			return nil, fmt.Errorf("%w: volume %s", errVolumeNotReady, name)
		}
		volumes[name] = compose.MountedVolume{Dir: dir, Name: lv}
	}
	return volumes, nil
}

func (r *Runner) render(desired desiredState, stack *state.Stack, volumes map[string]compose.MountedVolume,
	secrets stackSecrets,
) ([]byte, error) {
	// before the first configuration there is no Floating IP
	floatingIP, err := netip.ParseAddr(desired.config.Ingress.FloatingIP)
	if err != nil {
		return nil, fmt.Errorf("the Floating IP of the configuration: %w", err)
	}
	rendered, err := compose.Render(stack.Name, stack.Spec, compose.Target{
		Namespace: stack.Namespace, FloatingIP: floatingIP, NodeAddress: r.nodeAddress,
		Resolver: network.Resolver, Volumes: volumes, SecretsDir: r.files.secretFilesDir(stack.Project()),
		SecretKeys: secrets.keys, SecretLabels: secrets.labels, Restarted: stack.Restarted,
	})
	if err != nil {
		return nil, fmt.Errorf("render the Compose file: %w", err)
	}
	return rendered, nil
}

// up pulls and runs compose up with the registry logins in the Docker
// config directory config.
func (r *Runner) up(ctx context.Context, config string, stack *state.Stack, rendered []byte, secrets stackSecrets,
	volumes map[string]compose.MountedVolume,
) error {
	project := stack.Project()
	hash := upHash(rendered, secrets, stack.PushedOut)
	// read before compose up, so that a restart of Docker during compose up
	// makes the stack come up again at the next pass
	upNow := loop.BroughtUp{Hash: hash, DockerRestarts: r.signal.Restarts()}
	// at every pass, not only before compose up, so that unchanged files
	// get their owner and mode back
	if err := r.writeSecrets(project, secrets); err != nil {
		return err
	}
	if r.appliedWith(project) == upNow {
		return nil
	}
	files, err := r.writeStackFiles(project, rendered, secrets, volumes)
	if err != nil {
		return err
	}
	if err := r.pullRestarted(ctx, config, project, files, stack.Restarted); err != nil {
		return err
	}
	err = loop.WhilePaused(r.supervisor, project, func() error {
		return r.docker.ComposeUp(ctx, config, project, files, stack.PushedOut)
	})
	if err != nil {
		return err
	}
	r.setApplied(project, upNow)
	// the containers of the previous Compose file are gone, so the list of
	// LVs shrinks to the new ones. A longer list left by a failed write only
	// makes another stack wait a bit longer, so the stack still counts as up.
	if err := r.writeMountedLVs(project, lvsOf(volumes)); err != nil {
		r.logger.Warn("shorten the volumes a stack mounts", "project", project, "error", err)
	}
	return r.removeStaleSecrets(project, secrets)
}

// writeStackFiles returns the Compose files for compose up.
func (r *Runner) writeStackFiles(project string, rendered []byte, secrets stackSecrets,
	volumes map[string]compose.MountedVolume,
) ([]string, error) {
	// before the Compose file, so that a stack with a Compose file always
	// has its LVs listed too
	if err := r.addMountedLVs(project, lvsOf(volumes)); err != nil {
		return nil, err
	}
	file := r.files.stackFile(project)
	if err := atomicfile.Write(file, rendered, 0o600); err != nil {
		return nil, err // it names the file
	}
	files := []string{file}
	if secrets.env != nil {
		files = append(files, r.files.secretEnvFile(project))
	}
	return files, nil
}

func lvsOf(volumes map[string]compose.MountedVolume) []string {
	lvs := []string{}
	for _, volume := range volumes {
		lvs = append(lvs, volume.Name)
	}
	return lvs
}

// addMountedLVs keeps the LVs listed before, because the containers of the
// previous Compose file keep theirs until compose up succeeds.
//
// If the list cannot be read and there is no Compose file, the stack has no
// containers yet, and the list becomes just the new LVs. With a Compose
// file, the containers may hold any LV: the file is left as it is, and
// markInUse, which cannot read it either, counts every volume as in use.
func (r *Runner) addMountedLVs(project string, lvs []string) error {
	held, err := r.files.readMountedLVs(project)
	if err != nil {
		_, statErr := os.Stat(r.files.stackFile(project))
		if !errors.Is(statErr, os.ErrNotExist) {
			r.logger.Warn("every volume counts as the stack's until it is up", "project", project, "error", err)
			return nil
		}
	}
	return r.writeMountedLVs(project, append(held, lvs...))
}

func (r *Runner) writeMountedLVs(project string, lvs []string) error {
	lvs = slices.Compact(slices.Sorted(slices.Values(lvs)))
	encoded, err := json.Marshal(lvs)
	if err != nil {
		return fmt.Errorf("write the volumes of %s: %w", project, err)
	}
	return atomicfile.Write(r.files.stackVolumesFile(project), encoded, 0o600)
}

func (f Files) readMountedLVs(project string) ([]string, error) {
	var lvs []string
	content, err := os.ReadFile(f.stackVolumesFile(project))
	if err == nil {
		err = json.Unmarshal(content, &lvs)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.stackVolumesFile(project), err)
	}
	return lvs, nil
}

func (r *Runner) pullRestarted(ctx context.Context, config, project string, files []string,
	restarted map[string]time.Time,
) error {
	services, err := r.restartedSinceUp(ctx, project, restarted)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	if err := r.docker.ComposePull(ctx, config, project, files, services); err != nil {
		return fmt.Errorf("pull the images of %s: %w", strings.Join(services, ", "), err)
	}
	return nil
}

// restartedSinceUp finds the services to pull before compose up recreates
// them, because an image may have changed under the same tag. Docker keeps
// the label, so nothing is pulled again after a reboot or an agent restart.
// A service without a container gets its image from compose up anyway.
func (r *Runner) restartedSinceUp(ctx context.Context, project string, restarted map[string]time.Time) ([]string, error) {
	if len(restarted) == 0 {
		return nil, nil
	}
	containers, err := r.docker.Containers(ctx, compose.LabelStack+"="+project)
	if err != nil {
		return nil, err
	}
	var services []string
	for _, container := range containers {
		service := container.Labels[docker.ServiceLabel]
		at, ok := restarted[service]
		if ok && container.Labels[compose.LabelRestarted] != compose.RestartedLabel(at) {
			services = append(services, service)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(services))), nil
}

// upHash hashes everything that compose up reads. pushedOut must be sorted,
// as the state keeps it.
func upHash(rendered []byte, secrets stackSecrets, pushedOut []string) [32]byte {
	hash := sha256.New()
	digest.WriteParts(hash, rendered, secrets.env)
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		for _, key := range secrets.keys[name] {
			digest.WriteParts(hash, []byte(name), []byte(key), secrets.files[name][key])
		}
	}
	for _, service := range pushedOut {
		digest.WriteParts(hash, []byte("pushed out"), []byte(service))
	}
	return [32]byte(hash.Sum(nil))
}

// reportRunning reports nothing for a stack that runs with the same
// secrets: a new generation or a restart would have set it to starting.
func (r *Runner) reportRunning(ctx context.Context, stack *state.Stack, secrets stackSecrets) error {
	if stack.Status == state.StackRunning && maps.Equal(stack.AppliedSecrets, secrets.digests) {
		return nil
	}
	err := loop.CommitAsHost(ctx, r.propose, r.host, state.KindSetStackStatus, state.SetStackStatus{
		ID: stack.ID(), Status: state.StackRunning, Host: stack.Host, Generation: stack.Generation,
		Restarted: stack.Restarted, AppliedSecrets: secrets.digests,
	})
	if err != nil {
		return fmt.Errorf("report the stack running: %w", err)
	}
	return nil
}

// upErrorLength is how much of a failing stack's error the state keeps
// (Stack.UpError): enough to show the cause. The log has the rest.
const upErrorLength = 200

// reportFailing reports a stack as failing with the first line of the
// error, shortened. A stack that fails again keeps its first error, so a
// pass that fails again writes nothing. A failed report is only logged,
// because the next pass reports again.
func (r *Runner) reportFailing(ctx context.Context, stack *state.Stack, cause error) {
	if stack.Status == state.StackFailing {
		return
	}
	upError, _, _ := strings.Cut(cause.Error(), "\n")
	if runes := []rune(upError); len(runes) > upErrorLength {
		upError = string(runes[:upErrorLength-1]) + "…"
	}
	err := loop.CommitAsHost(ctx, r.propose, r.host, state.KindSetStackStatus, state.SetStackStatus{
		ID: stack.ID(), Status: state.StackFailing, Host: stack.Host, Generation: stack.Generation,
		Restarted: stack.Restarted, UpError: cmp.Or(upError, "an empty error"),
	})
	if err != nil {
		r.logger.Error("report the stack failing", "stack", stack.ID(), "error", err)
	}
}
