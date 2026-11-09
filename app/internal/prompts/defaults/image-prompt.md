<!-- Used by the "Tạo ảnh" button on a message: the chat model reads the scene and writes the picture's description, the image model draws it. {excerpt} = the last messages up to that one, {profiles} = profiles of the characters named in them, {player} = the player's character, {pictures} = characters who have a reference picture. The model may look the source up first (search_story, read_chapter, get_character) when it can call tools. Must end with JSON only. -->
Draw the moment the last message of this excerpt shows.

<excerpt>
{excerpt}
</excerpt>

<profiles>
{profiles}
</profiles>

Characters with a reference picture the drawing will follow:
{pictures}

If you need to know how someone or somewhere looks, or which page of the source shows them, look it up first. Then answer with one JSON object only, no other text:
{"prompt": "...", "characters": ["..."], "references": [{"chapter": 0, "page": 0}], "continue_previous": false, "aspect": "landscape"}

- prompt: a detailed picture description in English: who is in the frame and how each one looks (face, hair, eyes, build, clothes, from the profiles and the story), their poses and expressions, what is happening, the place, time of day, lighting, camera angle and framing. Keep the source's art style. Only what the excerpt shows at that moment: no extra characters or events.
- characters: everyone in the frame, written exactly as in the list above when they are in it ({player} included if they are in the frame), so their pictures are attached.
- references: pages of the source (chapter number from the story map, page number from read_chapter) that show this place or these characters well, if you know any; otherwise [].
- continue_previous: true when an earlier illustration in the excerpt shows this same place and moment.
- aspect: landscape, portrait or square.
