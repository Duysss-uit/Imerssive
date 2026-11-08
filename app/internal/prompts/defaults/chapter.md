<!-- Builds one entry of the story map. A chapter is read passage by passage (each passage sized to the model's context); every call updates the chapter's running summary. The reply must stay a JSON object with "summary" and "entities".
Placeholders: {title} name of the work, {chapter} chapter number and title, {part}/{parts} passage number and count, {pages} the passage's pages,
{story_so_far} the story map of the chapters before this one, {known} the characters met so far (name: looks [other labels]), {notes} this chapter's summary so far, {text} the passage.
"aliases" is how one character described by different labels on different pages (comic transcriptions label unnamed people by their looks: "[?] girl · black hair · glasses") ends up as one profile: keep it. -->
You are building the story map of the work "{title}": one short entry per chapter, in story order. Readers use the map to know what happens where, then look up the details in the original text.

<story_so_far>
{story_so_far}
</story_so_far>

<known_characters>
{known}
</known_characters>

Now reading {chapter}, passage {part} of {parts} (pages {pages}).

<chapter_summary_so_far>
{notes}
</chapter_summary_so_far>

<passage>
{text}
</passage>

Return ONLY a JSON object, no prose, in this shape:
{"summary": "...", "when": "...", "entities": [{"name": "...", "type": "character|place|faction|item|event|concept", "description": "...", "quotes": ["..."], "aliases": ["..."]}]}

- summary: the summary of this chapter from its start up to the end of this passage: the previous chapter summary updated with what this passage adds. Three to six sentences, in story order: the key events, who does what, decisions, revelations, where the chapter leaves things. Name characters exactly as the text does. Write it in the language of the work.
- when: when this chapter happens in the story's own timeline, as precisely as the text allows: a date, a day count, a season, an age, or relative to earlier chapters ("the next morning", "three years after chapter 4"). Use the story map so far to place it. "" if the text gives no clue.
- entities: the significant characters, places, factions or items in this passage. description: appearance, personality, abilities, relationships, and what they do or what happens to them here. quotes (characters only): up to two lines this character says in the passage that show how they talk, copied word for word; [] if they don't speak.
- Same person, one entry: the passage may call someone by a label that describes their looks ("[?] ...") or by a nickname, title or number. If that is a known character (same looks, same role) or the passage reveals their name, use their name for the entry and in the summary, and list every other label or name used for them, here or among the known characters, in "aliases". A character who is still unnamed keeps their label as the name. Only merge when you are fairly sure; two different people stay two entries.
