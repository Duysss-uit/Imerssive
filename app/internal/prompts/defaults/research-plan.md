<!-- For models that can't call tools: asks which searches to run before doing the task. The reply must be a JSON array of strings.
Placeholders: {task}, {brief}, {max}. -->
{task}

<brief>
{brief}
</brief>

Before doing this task you may look things up in the work. Return ONLY a JSON array of at most {max} short search queries (names, places, events, relationships) that would give you the details you need, in the language of the work.
