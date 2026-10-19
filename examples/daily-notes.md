# Daily note carryover

You are running in a directory of daily notes named
`daily-notes/YYYY-MM-DD.md`.

Carry yesterday's unfinished backlog into today's note.

1. Determine today's and yesterday's dates. Use the local date/time in your
   system prompt; run `date` if you need to confirm it. Daily note filenames are
   `daily-notes/YYYY-MM-DD.md`.
2. Read yesterday's daily note. If it does not exist, stop and say so in one
   sentence. Do not create a note in that case.
3. Read today's daily note if it already exists. Preserve its content and merge
   your additions into it rather than overwriting the file.
4. Find every unfinished TODO in yesterday's note: lines beginning with
   `- [ ]` (or `* [ ]`). Ignore completed items (`- [x]`).
5. Copy each unfinished item into a `## Carried over` section in today's note.
   - Do not duplicate an item that is already present in today's note.
   - Keep the item text intact, including any existing carryover marker.
6. Determine each item's carry-over age: the number of consecutive daily notes,
   ending with yesterday, in which that same unfinished item appears. Search the
   previous daily notes with `grep` rather than guessing.
7. If an item's age is greater than 3 days, append a marker to the copied item:
   ` 🔁 (<age> days)`. If the item already has a marker, update it to the new
   age instead of adding a second marker. Items aged 3 days or fewer get no
   marker.
8. Do not modify yesterday's note or any note other than today's. Do not delete
   anything.
9. When finished, reply with a short summary: how many items were carried over
   and how many are older than 3 days.

If there are no unfinished items, create today's note only if it is missing (a
minimal `# <date>` heading is fine) and reply that there was nothing to carry
over.
