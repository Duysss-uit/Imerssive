package store

// Provider is a user-configured LLM endpoint. The API key lives in the OS keyring.
type Provider struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	BaseURL     string  `json:"baseUrl"`
	Model       string  `json:"model"`
	Vision      bool    `json:"vision"`
	Temperature float64 `json:"temperature"`
	Effort      string  `json:"effort"`
	Thinking    string  `json:"thinking"` // "" = model default, "off" = ask the model not to think
	HasKey      bool    `json:"hasKey"`
}

// KnowledgeBase groups source documents (a novel, a manga series...) that
// characters and scenarios draw their lore from.
type KnowledgeBase struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Overview    string `json:"overview"` // auto-generated summary of all sources
	CreatedAt   int64  `json:"createdAt"`

	// Providers used to process this KB's documents; empty = the role default in settings.
	LLMProvider    string `json:"llmProvider"`
	VisionProvider string `json:"visionProvider"`
	// Embedding: empty EmbedProvider = the local fastembed model.
	EmbedProvider string `json:"embedProvider"`
	EmbedModel    string `json:"embedModel"`
}

type Source struct {
	ID        string `json:"id"`
	KBID      string `json:"kbId"`
	Filename  string `json:"filename"`
	Path      string `json:"path"`
	ComicMode string `json:"comicMode"` // vision | embed | both
	Status    string `json:"status"`    // queued | extracting | reading | summarizing | embedding | done | error
	Progress  string `json:"progress"`
	Error     string `json:"error"`
	Pages     int    `json:"pages"`
	CreatedAt int64  `json:"createdAt"`
}

type Character struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Avatar          string   `json:"avatar"` // data URI or empty
	Tagline         string   `json:"tagline"`
	Description     string   `json:"description"`
	Personality     string   `json:"personality"`
	SpeakingStyle   string   `json:"speakingStyle"`
	Greeting        string   `json:"greeting"`
	ExampleDialogue string   `json:"exampleDialogue"`
	KBIDs           []string `json:"kbIds"`
	CreatedAt       int64    `json:"createdAt"`
}

// CastMember is a character who appears in a scenario. The player can take
// one of these roles; the narrator voices the rest.
type CastMember struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Scenario is the "bối cảnh": where/when the roleplay takes place.
type Scenario struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Setting   string       `json:"setting"`
	Opening   string       `json:"opening"`
	Cast      []CastMember `json:"cast"`
	KBIDs     []string     `json:"kbIds"`
	Quick     bool         `json:"quick"` // made by "Chơi ngay" for one scene; hidden from the scenario list
	CreatedAt int64        `json:"createdAt"`
}

// Chat modes: one-on-one with a character card, or a scene where the player
// takes a role (PersonaName/PersonaDesc) and the model narrates everyone else.
const (
	ModeCharacter = ""
	ModeScene     = "scene"
)

type Chat struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	CharacterID string `json:"characterId"` // empty in scene mode
	ScenarioID  string `json:"scenarioId"`
	PersonaName string `json:"personaName"`
	PersonaDesc string `json:"personaDesc"`
	Title       string `json:"title"`
	ProviderID  string `json:"providerId"` // chat model; empty = default chat provider
	UpdatedAt   int64  `json:"updatedAt"`
}

type Message struct {
	ID         int64  `json:"id"`
	ChatID     string `json:"chatId"`
	Role       string `json:"role"` // user | assistant
	Content    string `json:"content"`
	Summarized bool   `json:"summarized"` // folded into long-term chat memory
	CreatedAt  int64  `json:"createdAt"`
}
