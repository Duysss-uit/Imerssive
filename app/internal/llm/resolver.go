package llm

// Roles a provider can be assigned to in settings. Users can point summarization
// at a cheap model and chat at a stronger one.
const (
	RoleChat      = "chat"
	RoleSummarize = "summarize"
	RoleVision    = "vision"
)

// Resolver returns the provider with the given id, or the one configured for
// the role in settings when id is empty.
type Resolver func(role, id string) (Provider, error)
