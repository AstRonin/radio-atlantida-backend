## Radio Station Requirements

* Online Broadcasting Capability: The system must support live online streaming.
* Remote Live Broadcasting: The ability to go on air live from a PC using a control panel or management block.
* Content Playlists: Dedicated playlists for managing different content, including music, jingles, news, features/segments, etc.
  * A playlist is a pool of tracks, not a queue. The order of tracks inside a playlist is not significant. The system chooses the track by the rotation rules.
  * A track may belong to several playlists.
  * Every playlist has a type. The default types are music, jingle, news, and segment.
  * Playlist types are dynamic. The manager can add a new type and assign it to playlists. A type that is in use cannot be deleted. The type is a label for the manager (filters and badges). It does not limit which playlists a clock can combine.
  * In the interface, the term is "Playlist" everywhere.
  * The manager can play any track directly in the admin panel (a play button next to the track in playlists and in the library), without looking for the file on disk. The preview plays in the browser only and never goes to air.
* Automated Scheduling: Auto-generation of broadcasting schedules for 24 hours in advance, ideally up to a month.
* Planning Horizons:
  * The clock grid (which clock plays in which hour) covers up to 30 days ahead and is edited explicitly by the operator.
  * The track plan (concrete tracks per hour) covers a rolling window of 24 to 48 hours and is extended automatically.
  * Liquidsoap holds only the next 2 to 3 tracks.
* Clock Rules & Rotation:
  * Each clock template must span 60 minutes.
  * A clock is a set of playlists and timed inserts. The system lays out the hour from them by itself; the operator does not list single tracks.
  * Every playlist has a placement rule for clocks: fill (fills the time left; the default), spread N tracks over the hour, at the start, at the end, at the start and the end, or in the middle (a track starting at about 30:00, ±5 minutes). A clock may override the rule of a playlist for that clock. A clock needs at least one fill playlist.
  * When a clock has several fill playlists, the operator may give each a share (weight); the music is then taken from them in that proportion. Either all fill playlists of a clock have a share or none; without shares every fill playlist has the same chance.
  * Tracks of any length are used. Audio is never time-stretched, and the track itself is never altered. The last track before an insert or the end of the hour is the one that fits the remaining time best; if none fits, it is cut with a short fade-out.
  * A timed insert plays a track, or a track from a playlist, at a set time of the hour with seconds (for example, 29:40) and a start tolerance (default ±1 minute), so an insert at 30:00 may begin anywhere from 29:00 to 31:00. Inserts close together play back to back without music between them (for example, a jingle at 29:40, the news at 30:00, and a jingle after it).
  * Inserts are set in the clock template (every hour of that clock) or added once to a single planned hour that has not started (for example, today's news). A one-time insert stays when the hour is rebuilt; the clock template is not changed. A one-time insert at the same time as a template insert replaces it for that hour.
  * There is no filler: the fill playlists fill the hour. If no fill track is allowed by the rules, the emergency steps apply and the operator is warned. Air never waits for the operator.
  * Validation on save: saving a clock builds a sample hour and shows it with its warnings (an empty playlist, a rule no track can satisfy, inserts that cannot be placed, a fill playlist without a minimum interval). Problems are shown immediately, not at air time.
  * Overflow: If a track runs past an insert's time or the end of the hour, that track is kept and fades out over 2 to 3 seconds (default 2.5 s), ending at that point. The tracks after it before that point are removed from the plan. This keeps the air filled without leaving a gap.
    * A truncated track counts as aired. It is logged with its actual played duration and counts toward play rule intervals like any other track.
    * Removed tracks are not aired and are not missed. They do not count toward play rule intervals and go back to their playlist.
  * Crossing the hour: The hour boundary is hard. Each clock ends exactly at the end of its hour, and the next clock starts exactly on time. A track that starts near the end of a clock is cut at the boundary and does not carry over.
  * Grid changes: Changing a clock, a playlist's placement rule, or the grid recalculates only planned, not yet aired items in the affected hours.
* Broadcast Failure Handling: If the broadcast is interrupted (for example, the Icecast server goes down, or the internet connection is lost), the system must record which scheduled items did not reach the air.
  * Interruptions are detected automatically and stored as outage periods with a start time, an end time, and the component that failed (for example, Icecast, the Liquidsoap control link, or the backend).
  * Items that were planned for an outage period and did not air are marked as "missed". Missed items are stored with their planned start time, planned duration, the clock and hour they belonged to, and the reason for the interruption.
  * The played-tracks list in the admin panel shows aired, truncated, and missed items together. Missed items are marked clearly (for example, a "Not aired" label), so the manager can see at a glance which programs or segments were not broadcast.
  * Missed items are kept until the manager resolves them. They are not removed by the normal retention period.
  * The manager can reschedule a missed item into a following hour. The manager chooses the placement (start, end, or a fixed time with seconds), and the hour is rebuilt around it like any other one-time insert. This is a one-time change for that hour; the clock template is not changed. If the item does not fit, the request is refused.
  * The manager can also dismiss a missed item if it should not be broadcast again.
  * If the outage ends while the backend is down, the system reconciles what actually aired from the playout log kept by Liquidsoap, so the played-tracks list is accurate after recovery.
* Grid Management: Ability to create various clock templates and assign them to specific hours, days, and timeslots, up to 30 days ahead.
* Play Rules: A separate page where the manager creates named, reusable play rules and applies them to tracks (mainly jingles) or to whole playlists. This replaces dayparting.
  * A rule may combine: a time window of the day with weekdays (for example, 06:00 to 08:00), a minimum interval between plays of the same track (for example, 10 minutes, or 72 hours), a minimum interval between tracks of the same artist (for example, 2 hours), and a maximum number of plays per hour (for example, 1).
  * Separation is a play rule: the default rule "Music rotation" (a track at most once per 72 hours; its artist interval is copied from the reference software) is attached to the music playlists. There are no separate separation rules. Only items that actually aired or are planned count; items marked as missed do not. An interval cannot be longer than the play log is kept.
  * Examples: one jingle plays only from 06:00 to 08:00 and another only from 10:00 to 12:00; a jingle plays at most once per hour; a jingle plays no more often than every 10 minutes.
  * A rule limits when a track may play. How often a playlist appears in the hour (for example, two jingles an hour) is set by its placement rule for clocks.
  * A track can have several rules, and all of them must pass. A track also follows the rules of its playlists.
  * Rules are checked at planning time, for every track, including inserts and rescheduled items. A rule is never relaxed. If no track passes, the slot is skipped and the operator is warned.
* Audio Quality Analysis: The system should automatically read a track's audio quality and format, providing a warning or recommendation if the quality falls below the required threshold.
* Audio Transcoding: Built-in tools to reformat/transcode audio files to match the required broadcast standard.
