package cluster

// Tag keys stored in serf node metadata.
const (
	TagKeyRole  = "role"
	TagKeyState = "state"
)

// Role values for TagKeyRole.
const (
	RoleSidecar = "sidecar"
	RoleTask    = "task"
)

// State values for TagKeyState.
const (
	StateStarting = "starting" // task: bound port, worker not yet ready
	StateReady    = "ready"    // task: worker confirmed accepting
)

// Query names — serf query name strings used as the RPC discriminator.
const (
	QueryAnnounce = "announce" // task-initiated registration
	QuerySolicit  = "solicit"  // sidecar-initiated re-announce request
)
