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
	// ContextWindow is the model's context length in tokens; 0 = detect it.
	ContextWindow int  `json:"contextWindow"`
	HasKey        bool `json:"hasKey"`
}

// KnowledgeBase groups source documents (a novel, a manga series...) that
// characters and scenarios draw their lore from.
type KnowledgeBase struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Overview    string `json:"overview"` // the story map: one entry per chapter, in story order
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
	Status    string `json:"status"`    // queued | extracting | reading | read (waiting to be embedded) | embedding | embedded (waiting to be summarized) | summarizing | done | error
	Progress  string `json:"progress"`
	Error     string `json:"error"`
	Pages     int    `json:"pages"`
	Seq       int    `json:"seq"`      // story order within the knowledge base
	Sections  int    `json:"sections"` // chapters it contributed to the story map
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
	// Divergence is how this story has departed from the source so far, kept
	// by the model every few turns and editable by the player.
	Divergence     string `json:"divergence"`
	DivergenceUpTo int64  `json:"-"` // last message the log accounts for
}

// Chapter is one entry of a work's story map: a chapter (or part of a long
// one) with a short summary and who appears in it.
type Chapter struct {
	ID       string   `json:"id"`
	KBID     string   `json:"kbId"`
	SourceID string   `json:"sourceId"`
	Idx      int      `json:"idx"` // order within its source
	No       int      `json:"no"`  // number in the whole work, from 1
	Title    string   `json:"title"`
	Pages    string   `json:"pages"`
	Summary  string   `json:"summary"`
	Cast     []string `json:"cast"`
}

type Message struct {
	ID         int64  `json:"id"`
	ChatID     string `json:"chatId"`
	Role       string `json:"role"` // user | assistant
	Content    string `json:"content"`
	Summarized bool   `json:"summarized"` // folded into long-term chat memory
	CreatedAt  int64  `json:"createdAt"`
}
