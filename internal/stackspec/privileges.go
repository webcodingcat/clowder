package stackspec

import "strings"

// harmlessCapabilities act only inside the container's own namespaces (its
// network, its processes): Docker's default set, and a few that services
// commonly add. Any other capability reaches the host.
var harmlessCapabilities = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "NET_RAW": true, "SETFCAP": true,
	"SETGID": true, "SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
	"NET_ADMIN": true, "NET_BROADCAST": true, "IPC_LOCK": true, "SYS_NICE": true,
}

// CapabilityName is a capability as Docker writes it: in upper case,
// without CAP_.
func CapabilityName(capability string) string {
	return strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
}

// HarmlessCapability tells whether a capability (with or without CAP_)
// acts only inside the container.
func HarmlessCapability(capability string) bool {
	return harmlessCapabilities[CapabilityName(capability)]
}

// HarmlessSecurityOpt tells whether a security option only takes privileges
// away.
func HarmlessSecurityOpt(option string) bool {
	return option == "no-new-privileges" || option == "no-new-privileges:true"
}

// Privileges lists what a service asks for beyond a normal container. Such
// a container can read the host's keys and other stacks' volumes and
// secrets, and it can change the network rules that keep containers apart.
// Only stacks that the configuration names in stacks.privileged may have
// privileges.
func (s Service) Privileges() []string {
	var asked []string
	if s.Privileged {
		asked = append(asked, "privileged")
	}
	for _, capability := range s.CapAdd {
		if !HarmlessCapability(capability) {
			asked = append(asked, "capAdd "+CapabilityName(capability))
		}
	}
	for _, option := range s.SecurityOpt {
		if !HarmlessSecurityOpt(option) {
			asked = append(asked, "securityOpt "+option)
		}
	}
	return asked
}

// Privileged tells whether a service of the stack has privileges.
func (s Spec) Privileged() bool {
	for _, service := range s.Services {
		if len(service.Privileges()) > 0 {
			return true
		}
	}
	return false
}
