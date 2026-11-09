package prompts

// previous holds the SHA-256 of every earlier version of each default prompt.
// A prompt file still equal to one of them was never edited by the user, so
// it is upgraded to the current default (or removed, when the prompt is no
// longer used). Edited files are never touched.
//
// When changing a default, add the hash of the version being replaced here
// (sha256sum defaults/<name>.md before the change).
var previous = map[string][]string{
	"image-prompt":      {"7edb7a5cfd37306d164d0583521b56c2e24ddbc6da012d0f5a4500324f53e463"},
	"chat-lore":         {"65e5bf6d8796e383a58079a0425113faccffc83aad6b6c226ba48a77c7d018b2"},
	"chat-reminder":     {"8fcaaa8e824dff91c7c04564490463a308572bdabd4836e3648a1b53b28c1b99"},
	"chat-character":    {"bd533d60567819e149a838043bf3b369bb94b197e00bb36a32c30db4eca25578", "7d7b66b9ad5f93ca954c8d4d5530de4d9c0c4a3c82f8630bd10a20a9531d9e79", "e1f9bd9a1f9c24f7251aa4b3daf19c3449f57258e3e119890f1912d09734d100"},
	"chat-narrator":     {"1b87dce81a6f7a76aa7edf33b54d3bb8a0efcbc5f5e0da776a752d633be5926b", "080a38ef98b6c8e83dbd6e2bea362898bdb45430a9f7ce5d2e56ab07c015d426", "eb0303cdebddc06842f450983676b8599fb99675ffc0d3b01efa2fe3a979237c", "5a23b56bc37a11457a9ded773d4d14bbee22a7f14f92e90766cb0267385612b5"},
	"character-card":    {"4c110ac375fdd1310e6e98a24cd7b1caa2943737c7059e22137e4551f2943ed7", "b0aef7b87b560051388bf9d8560201f59abce791cac7022a13315856442e8392"},
	"scenario":          {"f26cb8a1362536377838bd182694fa1c573ee0d90d7bb3d381aa770330e635af", "d91c01eadf833bb8831a10654e0210d1a6090cc600da7bea48352617b071180a"},
	"cast":              {"8396af8b322367a2076d32c266577b3e710e9afa5631c0ffdfd7fe767f0a1775"},
	"analyst-system":    {"2cdf2f3b58a188c591257194f166568117a32a29a307285a27ae368d8fe7c7dc"},
	"chat-divergence":   {"4959d6bc971eb2a986a6b2fb77b196ed1ce190668b363a37080645c5a1f036f1", "dd2fe2b277adaf0de0bf9b43c6fe08e7d1055f4199d0ade9ba6b4ed0049717c1"},
	"chapter":           {"96977c36e8e7b99e5e0603dfd84efc0418bc71f91d4fbfd949d63e487c9ff077", "a53cda5bc33d1dce5f2153e76d041dfff662889338f68ec0f3a4d2ac12ee404d", "ce884f7f0f64e9f91491d73b2bbcff4d8053d3d03eb39999a70a67a3649d2dd6"},
	"research-tools":    {"1801f9e25773ddb0acbb95f8b00564775997af747f3650108862eb7e1014d3b3"},
	"vision":            {"1f586426867c4d08d10b4e7fc2cee77e9b7688c31fb725498098fd2aec41582c", "eaea04bf2f520987ead9eee4f6854cc7f295987f625beb25b6bbebbc7bce78b6"},
	"section":           {"a0f2a3acd579e3d5be1d99a5e303710120ce4e8117c7f00660c22d8a3d190455"}, // retired: the story map replaced section summaries
	"story-so-far":      {"d20ba0e664dac56cf7be2824957d4f8259a371a3f71fe2c12a00bcd3c3e4558f"}, // retired
	"overview":          {"1114c5419134cd3b1bedeaa35ae6658fba08b7eb75cb21e6c805548f15811e81"}, // retired
	"overview-continue": {"b042c92d1eddc9732062a829fd64c0cc90d563138dcf32cb6df4d27ef6868321"}, // retired
}
