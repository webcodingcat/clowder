package state

import (
	"maps"
	"slices"
)

// Clone returns a copy of the state without its audit. It shares what the
// handlers only ever replace whole (the specs, a stack's Services, the
// encrypted values, the certificates' bytes), so nobody may change those in
// place. docs/developing/state.md, Copies of the state, explains why.
func (s *State) Clone() *State {
	copied := *s
	copied.Audit = nil
	copied.changes = nil
	copied.Keys = clonePointer(s.Keys)
	copied.JoinTokens = cloneMap(s.JoinTokens, func(token JoinToken) JoinToken {
		token.Roles = slices.Clone(token.Roles)
		return token
	})
	copied.Hosts = cloneMap(s.Hosts, cloneHost)
	copied.Forgetting = cloneMap(s.Forgetting, slices.Clone[[]HostID])
	copied.Ingress = clonePointer(s.Ingress)
	copied.ACMEAccount = clonePointer(s.ACMEAccount)
	copied.Challenges = maps.Clone(s.Challenges)
	copied.Certificates = cloneMap(s.Certificates, clonePointer[Certificate])
	copied.Revoked = maps.Clone(s.Revoked)
	copied.Namespaces = cloneMap(s.Namespaces, clonePointer[Namespace])
	copied.Stacks = cloneMap(s.Stacks, cloneStack)
	copied.Secrets = cloneMap(s.Secrets, clonePointer[Secret])
	copied.Volumes = cloneMap(s.Volumes, cloneVolume)
	copied.Retired = slices.Clone(s.Retired)
	copied.RetiredMinors = maps.Clone(s.RetiredMinors)
	copied.Alerts = cloneMap(s.Alerts, clonePointer[Alert])
	copied.Operations = cloneMap(s.Operations, cloneOperation)
	copied.Locks = maps.Clone(s.Locks)
	return &copied
}

func cloneHost(host *Host) *Host {
	copied := *host
	copied.Roles = slices.Clone(host.Roles)
	copied.Pools = maps.Clone(host.Pools)
	copied.SplitBrain = slices.Clone(host.SplitBrain)
	copied.NoQuorum = slices.Clone(host.NoQuorum)
	copied.FencedAt = slices.Clone(host.FencedAt)
	return &copied
}

// cloneStack shares the spec and the Services, which putStack replaces
// together.
func cloneStack(stack *Stack) *Stack {
	copied := *stack
	copied.Host = clonePointer(stack.Host)
	copied.AppliedSecrets = maps.Clone(stack.AppliedSecrets)
	copied.PushedOut = slices.Clone(stack.PushedOut)
	copied.Restarted = maps.Clone(stack.Restarted)
	copied.MoveTo = clonePointer(stack.MoveTo)
	return &copied
}

func cloneVolume(volume *Volume) *Volume {
	copied := *volume
	copied.Minor = clonePointer(volume.Minor)
	copied.Copies = slices.Clone(volume.Copies)
	return &copied
}

func cloneOperation(operation *Operation) *Operation {
	copied := *operation
	copied.Locks = slices.Clone(operation.Locks)
	copied.Owner = clonePointer(operation.Owner)
	if operation.Move != nil {
		move := *operation.Move
		move.From = clonePointer(operation.Move.From)
		copied.Move = &move
	}
	if operation.Fence != nil {
		fence := *operation.Fence
		fence.Stacks = slices.Clone(operation.Fence.Stacks)
		copied.Fence = &fence
	}
	return &copied
}

// clonePointer copies the value that a pointer points to, one level deep.
func clonePointer[Value any](pointer *Value) *Value {
	if pointer == nil {
		return nil
	}
	copied := *pointer
	return &copied
}

// cloneMap copies a map and each of its values. It returns nil for nil, so
// a clone encodes the same way as the state.
func cloneMap[Key comparable, Value any](original map[Key]Value, clone func(Value) Value) map[Key]Value {
	if original == nil {
		return nil
	}
	copied := make(map[Key]Value, len(original))
	for key, value := range original {
		copied[key] = clone(value)
	}
	return copied
}
