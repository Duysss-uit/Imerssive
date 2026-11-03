package ingest

const analystSystem = `You are a meticulous literary analyst building a knowledge base that a roleplay AI will search later.
Be faithful to the source: never invent events, names or traits. Keep proper names exactly as written.
Always write in the same language as the source text.
Lengths are rough guides: never count words or sentences, and don't draft the answer twice. Decide, then write the output directly.`

const sectionPrompt = `Below is part %d of %d of the work "%s".

Return ONLY a JSON object, no prose, in this shape:
{"summary": "...", "entities": [{"name": "...", "type": "character|place|faction|item|event|concept", "description": "..."}]}

- summary: a few paragraphs, chronological. Key events, decisions, relationships, revelations, how characters feel and change. Include 1-2 short characteristic quotes if present.
- entities: only the significant ones in this part. description: appearance, personality, abilities, relationships, and what they do or what happens to them in this part.

<text>
%s
</text>`

const reducePrompt = `These are consecutive section summaries of the work "%s".
Write a single overview of several paragraphs covering: premise; the world and its rules; main characters and their relationships; the major story arcs in order; the current state of affairs at the end.
Output only the overview text.

%s`

const entityMergePrompt = `Merge these notes about "%s" (%s) from different parts of the work "%s" into one coherent profile of a few paragraphs at most.
Keep it chronological where relevant (how they change), keep concrete details (appearance, abilities, relationships, speech habits). Output only the profile text.

%s`

const visionPrompt = `These images are consecutive pages %d to %d of the comic/manga "%s".
For EACH page:
- transcribe all dialogue, narration and sound effects, naming the speaker when identifiable;
- describe the key actions, characters present, setting and emotions shown in the panels.
Manga is usually read right-to-left; follow the correct reading order.
Use exactly this format, one block per page, in the language of the dialogue:
=== PAGE <number> ===
<content>`

// The generation prompts below describe the task only. The knowledge comes
// separately: a brief (overview + names) and whatever the model looks up with
// the search_story tool, or a <knowledge> block when tools aren't available.

const characterPrompt = `Create a roleplay character card for "%s" from the work.
%s
Return ONLY a JSON object with these string fields:
{"name": "", "tagline": "one-line hook", "description": "who they are, appearance, background, situation (two short paragraphs)",
 "personality": "traits, values, fears, quirks (one paragraph)", "speakingStyle": "how they talk: tone, verbal tics, forms of address, sample phrases",
 "greeting": "the first message they send to start a roleplay, in character, a few sentences, with *actions* in asterisks",
 "exampleDialogue": "3-4 short exchanges formatted as {{user}}: ... / {{char}}: ..."}
Stay faithful to the source. Write in the same language as the work.`

const castFields = `"cast": [{"name": "exact name as in the source", "description": "who they are, appearance, personality, abilities, how they speak, their goal and state of mind in this scene, relationships to the others (one short paragraph)"}]`

const castRules = `cast: 3-8 characters from the work who are present in or plausibly drawn into this scene, most important first. Use only characters that exist in the work; never invent new ones. The player may choose to play any of them, so describe each from a neutral point of view.`

const scenarioPrompt = `Create a roleplay scenario (setting) from the work for the player to step into.
%s
Return ONLY a JSON object:
{"title": "", "setting": "time, place, situation, what is at stake, relevant world rules (one or two paragraphs)",
 "opening": "an immersive opening narration that sets the scene, a short paragraph",
 ` + castFields + `}
- opening: the player will pick their role only after this is written, so don't assume who they are: describe the scene and the cast, then end on a moment that invites action.
- ` + castRules + `
Stay consistent with the source. Write in the same language as the work.`

const castPrompt = `List the characters from the work for this roleplay scenario.

<scenario title=%q>
%s
</scenario>

Return ONLY a JSON object:
{` + castFields + `}
- ` + castRules + `
Write in the same language as the work.`

// researchNote follows the task when the model can search the work itself.
const researchNote = `<brief>
%s
</brief>

The brief is only an outline of the work. Before answering, use the search_story tool to look up the specific details this task needs (characters, places, events, relationships), at most %d searches, then return the JSON.`

// planPrompt asks for search queries when the model can't call tools.
const planPrompt = `%s

<brief>
%s
</brief>

Before doing this task you may look things up in the work. Return ONLY a JSON array of at most %d short search queries (names, places, events, relationships) that would give you the details you need, in the language of the work.`

// knowledgeNote follows the task with the knowledge found for it.
const knowledgeNote = `<brief>
%s
</brief>

<knowledge>
%s
</knowledge>`
