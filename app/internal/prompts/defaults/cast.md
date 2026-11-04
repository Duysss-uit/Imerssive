<!-- Task for "✨ Trích xuất từ truyện" on a scenario the user wrote. The JSON field names must stay as they are. Placeholders: {title}, {setting}. -->
List the characters from the work for this roleplay scenario.

<scenario title="{title}">
{setting}
</scenario>

Return ONLY a JSON object:
{"cast": [{"name": "exact name as in the source", "description": "who they are, appearance, personality, abilities, how they speak, their goal and state of mind in this scene, relationships to the others (one short paragraph)"}]}
- cast: 3-8 characters from the work who are present in or plausibly drawn into this scene, most important first. Use only characters that exist in the work; never invent new ones. The player may choose to play any of them, so describe each from a neutral point of view.
Write in the same language as the work.
