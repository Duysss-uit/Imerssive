<!-- Keeps a chat's divergence log: how the story has departed from the source because of the player's choices. Runs every few replies (Cài đặt → "Cập nhật sổ thay đổi sau ... lượt") with the summarize model; the player can also edit the log.
The reply replaces the whole log. Placeholders: {other} the character (or narrator), {player} the player's character, {canon} the story map of the source, {log} the current log, {excerpt} the new messages. -->
You keep the divergence log of a roleplay set in a known story. {player} is played by a person; {other} is played by the model. The story may follow the source or depart from it because of {player}'s choices.

<source_story_map>
{canon}
</source_story_map>

<current_log>
{log}
</current_log>

<new_messages>
{excerpt}
</new_messages>

Rewrite the log so it covers everything up to the end of the new messages. Record only what is true in THIS story and matters for continuing it:
- Where and when the story is now, and who is present.
- Events that went differently from the source, or that the source doesn't have.
- Changed states: injured, dead, captured, gone, transformed, secrets revealed, items gained or lost.
- Changed relationships, attitudes, promises, debts, plans in motion.
- What {player} has done and become known for.

Keep earlier entries that still hold; update or drop those that changed. Don't restate what simply follows the source. Write short bullet points under these headings: "Now", "Different from the source", "States", "Relationships". At most about fifteen bullets in all. Write in the language {player} writes in (English if unsure). Output only the log.
