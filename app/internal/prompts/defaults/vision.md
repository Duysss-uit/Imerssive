<!-- Reads ONE comic/manga page. Runs once per page, many pages in parallel, with thinking turned off.
The reply is stored as the page's text: it is searched, read back by the read_chapter tool, and used to build the story map.
Placeholders: {page} page number, {title} name of the work. -->
This image is page {page} of the comic/manga "{title}". Transcribe it for someone who cannot see it, following the panels in reading order (manga: right to left, top to bottom).

Use exactly this layout, in the language of the dialogue, and nothing else:
Characters: who appears, separated by ";". Use their name when the page shows or says it. Otherwise write a label in exactly this form, built only from what doesn't change from page to page: [?] gender · hair · most distinctive feature (glasses, scar, eye color, build...) — for example "[?] girl · long black hair · glasses". Don't put clothes, pose, expression or mood in a label.
Setting: place, time and mood, in one line
Panels:
1. what happens: actions, expressions, important objects
   Speaker: "exact line"
   (narration) "caption text"
   SFX: sound effects
2. ...

For Speaker, write the name or exactly the same label as on the Characters line. Copy every line of dialogue and narration word for word. Write only what the page shows; don't guess what happens elsewhere in the story. For a page with no story (cover, credits, ads), write "Panels: none" and what the page is.
