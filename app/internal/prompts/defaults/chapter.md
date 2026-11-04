<!-- Builds one entry of the story map. A chapter is read passage by passage (each passage sized to the model's context); every call updates the chapter's running summary. The reply must stay a JSON object with "summary" and "entities".
Placeholders: {title} name of the work, {chapter} chapter number and title, {part}/{parts} passage number and count, {pages} the passage's pages,
{story_so_far} the story map of the chapters before this one, {notes} this chapter's summary so far, {text} the passage. -->
You are building the story map of the work "{title}": one short entry per chapter, in story order. Readers use the map to know what happens where, then look up the details in the original text.

<story_so_far>
{story_so_far}
</story_so_far>

Now reading {chapter}, passage {part} of {parts} (pages {pages}).

<chapter_summary_so_far>
{notes}
</chapter_summary_so_far>

<passage>
{text}
</passage>

Return ONLY a JSON object, no prose, in this shape:
{"summary": "...", "entities": [{"name": "...", "type": "character|place|faction|item|event|concept", "description": "..."}]}

- summary: the summary of this chapter from its start up to the end of this passage: the previous chapter summary updated with what this passage adds. Three to six sentences, in story order: the key events, who does what, decisions, revelations, where the chapter leaves things. Name characters exactly as the text does. Write it in the language of the work.
- entities: the significant characters, places, factions or items in this passage. description: appearance, personality, abilities, relationships, and what they do or what happens to them here.
