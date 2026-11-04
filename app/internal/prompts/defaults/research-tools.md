<!-- Follows a generation task when the model can look the work up itself with the tools search_story, read_chapter and get_character.
Placeholders: {brief} story map + names in the work, {max} number of lookups allowed. -->
<brief>
{brief}
</brief>

The brief is only an outline of the work: the story map (chapters in order) and who has a profile. Before answering, look up the specific details this task needs, at most {max} lookups: get_character for a profile, search_story for names, places, events and relationships, read_chapter for what exactly happens in a chapter of the map. Then return the JSON.
