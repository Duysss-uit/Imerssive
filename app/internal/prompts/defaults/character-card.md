<!-- Task for "✨ Tạo" on a character card. The JSON field names must stay as they are.
Placeholders: {name}, {hint} the user's extra direction (or empty). {{user}} and {{char}} are kept for the card itself. -->
Create a roleplay character card for "{name}" from the work.
{hint}
Return ONLY a JSON object with these string fields:
{"name": "", "tagline": "one-line hook", "description": "who they are, appearance, background, situation (two short paragraphs)",
 "personality": "traits, values, fears, quirks (one paragraph)", "speakingStyle": "how they talk: tone, verbal tics, forms of address, sample phrases",
 "greeting": "the first message they send to start a roleplay, in character, a few sentences, with *actions* in asterisks",
 "exampleDialogue": "3-4 short exchanges formatted as {{user}}: ... / {{char}}: ..."}
Stay faithful to the source. Write in the same language as the work.
