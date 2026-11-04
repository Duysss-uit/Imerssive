package prompts

// previous holds the SHA-256 of every earlier version of each default prompt.
// A prompt file still equal to one of them was never edited by the user, so
// it is upgraded to the current default (or removed, when the prompt is no
// longer used). Edited files are never touched.
//
// When changing a default, add the hash of the version being replaced here
// (sha256sum defaults/<name>.md before the change).
var previous = map[string][]string{
	"chat-character":    {"bd533d60567819e149a838043bf3b369bb94b197e00bb36a32c30db4eca25578"},
	"chat-narrator":     {"1b87dce81a6f7a76aa7edf33b54d3bb8a0efcbc5f5e0da776a752d633be5926b"},
	"research-tools":    {"1801f9e25773ddb0acbb95f8b00564775997af747f3650108862eb7e1014d3b3"},
	"vision":            {"1f586426867c4d08d10b4e7fc2cee77e9b7688c31fb725498098fd2aec41582c"},
	"section":           {"a0f2a3acd579e3d5be1d99a5e303710120ce4e8117c7f00660c22d8a3d190455"}, // retired: the story map replaced section summaries
	"story-so-far":      {"d20ba0e664dac56cf7be2824957d4f8259a371a3f71fe2c12a00bcd3c3e4558f"}, // retired
	"overview":          {"1114c5419134cd3b1bedeaa35ae6658fba08b7eb75cb21e6c805548f15811e81"}, // retired
	"overview-continue": {"b042c92d1eddc9732062a829fd64c0cc90d563138dcf32cb6df4d27ef6868321"}, // retired
}
