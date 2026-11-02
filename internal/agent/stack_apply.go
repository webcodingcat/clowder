package agent

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
	"github.com/webcodingcat/clowder/internal/compose"
	"github.com/webcodingcat/clowder/internal/digest"
	"github.com/webcodingcat/clowder/internal/docker"
	"github.com/webcodingcat/clowder/internal/names"
	"github.com/webcodingcat/clowder/internal/network"
	"github.com/webcodingcat/clowder/internal/state"
	"github.com/webcodingcat/clowder/internal/storage"
)

// apply runs one stack: it opens its secrets, finds its volume copies,
// renders its Compose file, makes the copies, brings the stack up and
// reports it running. inUse are the LVs that a stack which failed to stop
// (stopLeavers) may still write to, so no other stack may take them yet.
func (r *stackRunner) apply(ctx context.Context, desired desiredState, stack *state.Stack, inUse volumesInUse) error {
	secrets, err := r.openSecrets(desired, stack)
	if err != nil {
		return err
	}
	volumes, err := r.stackVolumes(desired, stack, inUse)
	if err != nil {
		return err
	}
	// rendered before any volume is made, so a stack that cannot run leaves
	// nothing behind
	rendered, err := r.render(desired, stack, volumes, secrets)
	if err != nil {
		return err
	}
	if err := r.ensureVolumes(ctx, volumes); err != nil {
		return err
	}
	if err := r.up(ctx, stack, rendered, secrets, volumes); err != nil {
		return err
	}
	return r.reportRunning(ctx, stack, secrets)
}

// stackVolumes are this host's copies of the volumes a stack names, by
// name. It returns errVolumeInUse when a stack that failed to stop may
// still use one of them.
func (r *stackRunner) stackVolumes(desired desiredState, stack *state.Stack, inUse volumesInUse) (map[string]storage.Volume, error) {
	volumes := map[string]storage.Volume{}
	for _, name := range stack.Spec.VolumeNames() {
		volume, err := r.describe(desired, names.ID(stack.Namespace, name))
		if err != nil {
			return nil, err
		}
		if user, ok := inUse.user(volume.LV); ok {
			return nil, fmt.Errorf("%w: volume %s, by %s", errVolumeInUse, name, user)
		}
		volumes[name] = volume
	}
	return volumes, nil
}

// render renders a stack's Compose file for this host.
func (r *stackRunner) render(desired desiredState, stack *state.Stack, volumes map[string]storage.Volume,
	secrets stackSecrets,
) ([]byte, error) {
	// before the first cluster apply there is no Floating IP
	floatingIP, err := netip.ParseAddr(desired.config.Ingress.FloatingIP)
	if err != nil {
		return nil, fmt.Errorf("the Floating IP of cluster.yaml: %w", err)
	}
	mounted := map[string]compose.MountedVolume{}
	for name, volume := range volumes {
		mounted[name] = compose.MountedVolume{Dir: r.storage.MountPoint(volume), Name: volume.LV}
	}
	rendered, err := compose.Render(stack.Name, stack.Spec, compose.Target{
		Namespace: stack.Namespace, FloatingIP: floatingIP, NodeAddress: r.host.NodeAddress,
		Resolver: network.Resolver, Volumes: mounted, SecretsDir: r.files.secretFilesDir(stack.Project()),
		SecretLabels: secrets.labels, Restarted: stack.Restarted,
	})
	if err != nil {
		return nil, fmt.Errorf("render the Compose file: %w", err)
	}
	return rendered, nil
}

// ensureVolumes makes a stack's volume copies. They are recorded before they
// exist, so a copy made by a crashed pass is still found and removed later.
func (r *stackRunner) ensureVolumes(ctx context.Context, volumes map[string]storage.Volume) error {
	if err := r.recordCopies(slices.Collect(maps.Values(volumes))); err != nil {
		return err
	}
	for _, volume := range volumes {
		if err := r.storage.Ensure(ctx, volume); err != nil {
			return fmt.Errorf("make volume %s: %w", volume.ID, err)
		}
	}
	return nil
}

// up writes a stack's files and runs compose up without the pushed-out
// services, unless all of it came up before. It pulls the images of the
// services a user restarted since their containers came up first. A failed
// pull or compose up is tried again at the next pass.
func (r *stackRunner) up(ctx context.Context, stack *state.Stack, rendered []byte, secrets stackSecrets,
	volumes map[string]storage.Volume,
) error {
	project := stack.Project()
	hash := upHash(rendered, secrets, stack.PushedOut)
	// read before compose up: a restart of Docker during it makes the
	// stack come up again at the next pass
	upNow := broughtUp{hash: hash, dockerRestarts: r.signal.restarts()}
	if r.applied[project] == upNow {
		return nil
	}
	files, err := r.writeStackFiles(project, rendered, secrets, volumes)
	if err != nil {
		return err
	}
	if err := r.pullRestarted(ctx, project, files, stack.Restarted); err != nil {
		return err
	}
	err = whilePaused(r.supervisor, project, func() error {
		return r.docker.ComposeUp(ctx, project, files, stack.PushedOut)
	})
	if err != nil {
		return err
	}
	r.applied[project] = upNow
	// The containers of the last Compose file are gone, so the list of LVs
	// shrinks to the new ones. If that write fails, the longer list stays:
	// it only makes another stack wait for a volume a bit longer, so the
	// stack still counts as up.
	if err := r.writeMountedLVs(project, lvsOf(volumes)); err != nil {
		r.logger.Warn("shorten the volumes a stack mounts", "project", project, "error", err)
	}
	return r.removeStaleSecrets(project, secrets)
}

// writeStackFiles writes what compose up reads: the LVs the stack mounts
// (addMountedLVs), its Compose file and its secrets. It returns the
// Compose files, the stack's and, if it has secret variables, theirs.
func (r *stackRunner) writeStackFiles(project string, rendered []byte, secrets stackSecrets,
	volumes map[string]storage.Volume,
) ([]string, error) {
	// before the Compose file, so a stack that has one has its LVs too
	if err := r.addMountedLVs(project, lvsOf(volumes)); err != nil {
		return nil, err
	}
	file := r.files.stackFile(project)
	if err := atomicfile.Write(file, rendered, 0o600); err != nil {
		return nil, err // it names the file
	}
	if err := r.writeSecrets(project, secrets); err != nil {
		return nil, err
	}
	files := []string{file}
	if secrets.env != nil {
		files = append(files, r.files.secretEnvFile(project))
	}
	return files, nil
}

// lvsOf are the LVs of a stack's volumes.
func lvsOf(volumes map[string]storage.Volume) []string {
	lvs := []string{}
	for _, volume := range volumes {
		lvs = append(lvs, volume.LV)
	}
	return lvs
}

// addMountedLVs adds the LVs a stack is about to mount to its
// volumes.json (stackVolumesFile), the LVs its containers may hold: if the
// stack fails to stop once it left this host, no other stack may take
// them (markInUse). The LVs listed before stay, as the containers of the
// last Compose file keep theirs until compose up worked; up then lists
// only the new ones.
//
// When the list does not read, it depends on the Compose file. Without
// one, the stack has no containers yet, and the list is just the new LVs.
// With one, its containers may hold any LV: the file is left as it is,
// and markInUse, which cannot read it either, counts every volume as in
// use until compose up worked.
func (r *stackRunner) addMountedLVs(project string, lvs []string) error {
	held, err := r.readMountedLVs(project)
	if err != nil {
		_, statErr := os.Stat(r.files.stackFile(project))
		if !errors.Is(statErr, os.ErrNotExist) {
			r.logger.Warn("every volume counts as the stack's until it is up", "project", project, "error", err)
			return nil
		}
	}
	return r.writeMountedLVs(project, append(held, lvs...))
}

// writeMountedLVs writes the LVs a stack's containers may hold, sorted.
func (r *stackRunner) writeMountedLVs(project string, lvs []string) error {
	lvs = slices.Compact(slices.Sorted(slices.Values(lvs)))
	encoded, err := json.Marshal(lvs)
	if err != nil {
		return fmt.Errorf("write the volumes of %s: %w", project, err)
	}
	return atomicfile.Write(r.files.stackVolumesFile(project), encoded, 0o600)
}

// readMountedLVs reads the LVs a stack's containers may hold.
func (r *stackRunner) readMountedLVs(project string) ([]string, error) {
	var lvs []string
	content, err := os.ReadFile(r.files.stackVolumesFile(project))
	if err == nil {
		err = json.Unmarshal(content, &lvs)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.files.stackVolumesFile(project), err)
	}
	return lvs, nil
}

// pullRestarted pulls the images of the services a user restarted since
// their containers came up (restartedSinceUp).
func (r *stackRunner) pullRestarted(ctx context.Context, project string, files []string,
	restarted map[string]time.Time,
) error {
	services, err := r.restartedSinceUp(ctx, project, restarted)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}
	if err := r.docker.ComposePull(ctx, project, files, services); err != nil {
		return fmt.Errorf("pull the images of %s: %w", strings.Join(services, ", "), err)
	}
	return nil
}

// restartedSinceUp are the services, sorted, whose containers carry another
// restart time than the state's (compose.LabelRestarted): a user restarted
// them since. compose up recreates them for the new label; their images are
// pulled before, as one may have changed under the same tag. Docker keeps
// the label, so after a reboot or an agent restart nothing is pulled again,
// and a service without a container gets its image from compose up anyway.
func (r *stackRunner) restartedSinceUp(ctx context.Context, project string, restarted map[string]time.Time) ([]string, error) {
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

// upHash covers everything compose up reads: the rendered file, the secret
// variables, the secret files and the services left out (the state keeps
// them sorted).
func upHash(rendered []byte, secrets stackSecrets, pushedOut []string) [32]byte {
	hash := sha256.New()
	digest.WriteParts(hash, rendered, secrets.env)
	for _, name := range slices.Sorted(maps.Keys(secrets.files)) {
		digest.WriteParts(hash, []byte(name), secrets.files[name])
	}
	for _, service := range pushedOut {
		digest.WriteParts(hash, []byte("pushed out"), []byte(service))
	}
	return [32]byte(hash.Sum(nil))
}

// reportRunning reports a stack running with the secrets its containers
// came up with. A new generation or a restart made the stack starting, so
// running with the same secrets means nothing new to report.
func (r *stackRunner) reportRunning(ctx context.Context, stack *state.Stack, secrets stackSecrets) error {
	if stack.Status == state.StackRunning && maps.Equal(stack.AppliedSecrets, secrets.digests) {
		return nil
	}
	err := commitAsHost(ctx, r.propose, r.host.ID, state.KindSetStackStatus, state.SetStackStatus{
		ID: stack.ID(), Status: state.StackRunning, Host: stack.Host, Generation: stack.Generation,
		Restarted: stack.Restarted, AppliedSecrets: secrets.digests,
	})
	if err != nil {
		return fmt.Errorf("report the stack running: %w", err)
	}
	return nil
}

// upErrorLength is how much of an error the state keeps of a failing
// stack (Stack.UpError): enough to tell the cause; the log has the rest.
const upErrorLength = 200

// reportFailing reports a stack failing with the first line of the error,
// shortened. A stack that fails again keeps its first error, so a pass that
// tries in vain writes nothing. A failed report is only logged: the next
// pass reports again.
func (r *stackRunner) reportFailing(ctx context.Context, stack *state.Stack, cause error) {
	if stack.Status == state.StackFailing {
		return
	}
	upError, _, _ := strings.Cut(cause.Error(), "\n")
	if runes := []rune(upError); len(runes) > upErrorLength {
		upError = string(runes[:upErrorLength-1]) + "…"
	}
	err := commitAsHost(ctx, r.propose, r.host.ID, state.KindSetStackStatus, state.SetStackStatus{
		ID: stack.ID(), Status: state.StackFailing, Host: stack.Host, Generation: stack.Generation,
		Restarted: stack.Restarted, UpError: cmp.Or(upError, "an empty error"),
	})
	if err != nil {
		r.logger.Error("report the stack failing", "stack", stack.ID(), "error", err)
	}
}
