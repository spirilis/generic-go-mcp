---
name: timezone-report
description: Produce a short "what time is it on the team" report using this server's date tool, formatted the way the on-call handover expects.
license: Apache-2.0
metadata:
  version: 1.0.0
---

# Timezone report

A procedure written against *this server's* tools, which is the point of serving a skill
over MCP: generic "just check the time" advice needs no skill, but the exact call sequence
and output format for this endpoint does.

## Steps

1. Call the `date` tool once per timezone in `references/timezones.md`, passing the IANA
   name as the `timezone` argument. Do not guess offsets — daylight saving rules change and
   the tool already knows them.
2. If a call fails, report that row as `unavailable` and keep going. One bad timezone must
   not sink the report.
3. Render the results as a table ordered west to east, so the reader scans it the way the
   working day moves.

## Output

| Region | Local time | Working hours? |
| --- | --- | --- |
| ... | ... | yes / no |

"Working hours" is 09:00–17:00 local, Monday to Friday.
