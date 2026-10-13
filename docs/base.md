## Radio Station Requirements

* Online Broadcasting Capability: The system must support live online streaming.
* Remote Live Broadcasting: The ability to go on air live from a PC using a control panel or management block.
* Content Playlists: Dedicated playlists for managing different content, including music, jingles, news, features/segments, etc.
  * A playlist is a pool of tracks, not a queue. The order of tracks inside a playlist is not significant. The system chooses the track by the rotation rules.
  * A track may belong to several playlists.
  * Every playlist has a type. The default types are music, jingle, news, and segment.
  * Playlist types are dynamic. The manager can add a new type and assign it to playlists. A type that is in use cannot be deleted. The type is a label for the manager (filters and badges). It does not limit what a clock element can combine.
  * In the interface, the term is "Playlist" everywhere.
  * The manager can play any track directly in the admin panel (a play button next to the track in playlists and in the library), without looking for the file on disk. The preview plays in the browser only and never goes to air.
* Automated Scheduling: Auto-generation of broadcasting schedules for 24 hours in advance, ideally up to a month.
* Planning Horizons:
  * The clock grid (which clock plays in which hour) covers up to 30 days ahead and is edited explicitly by the operator.
  * The track plan (concrete tracks per hour) covers a rolling window of 24 to 48 hours and is extended automatically.
  * Liquidsoap holds only the next 2 to 3 tracks.
* Clock Rules & Rotation:
  * Each clock template must span 60 minutes.
  * A clock element refers to one or more playlists. When an element has several playlists, the track is chosen from all of them together, and the usual rules (separation, play rules, duration fit) apply to each candidate. The playlists of one element may be of different types (for example, music and jingle together).
  * Clock fitting without changing tracks: each clock element has a target duration and a tolerance (`duration_tolerance_ms`). When selecting a track, the system chooses the one that fits the remaining time best within that tolerance. Audio is never time-stretched, and the track itself is never altered.
  * Element placement: when an element is added to a clock, the operator chooses where it goes:
    * at the start of the clock;
    * at the end of the clock;
    * at a fixed minute of the hour (for example, :30). A fixed element has a tolerance of ±1 minute on its start, so a fixed element at :30 may begin anywhere from :29 to :31.
  * A fixed element splits the hour into segments. The clock is rebuilt around each fixed element: the tracks before it fill the segment before it, and the tracks after it fill the rest of the hour. Each segment is fitted separately.
  * If a fixed element cannot be placed within its tolerance (for example, the material before it does not fit into the segment), the clock is not saved and the operator is shown which element causes the problem.
  * Replacing a fixed element: if the operator adds a fixed element at the same time as an existing fixed element (within the tolerance), the new element replaces the existing one, and the clock is rebuilt once. The result shows which element was replaced.
  * Filling a short clock: If the tracks do not fill the 60-minute clock, the system adds one extra track from a source defined for that clock or element.
    * When a clock is saved short, a blocking dialog appears immediately. It shows the shortfall and asks the operator to choose the filler mode: a specific track or tracks (added to a pool for this clock), a playlist, an artist, a random track, or ask at air time. If several segments are short, the dialog covers each of them.
    * A choice is stored with the clock. The clock then needs no further questions.
    * A specific track choice is a pool attached to the clock, not to a single air date. When the clock needs filler, the next track from the pool is used in order. When the pool is empty, the clock's default mode applies and the operator is reminded to refill the pool.
    * A pool track is still checked against separation rules and play rules. If it violates a rule at the time of selection, it is skipped and the operator is warned.
    * If the operator closes the dialog without choosing, the clock is saved as incomplete and shows a warning.
    * Reminders are always created for incomplete clocks, for each affected hour in the planning window. A reminder is closed when the operator answers or when its deadline passes. The default mode then applies (usually random).
    * Air never waits for an answer.
  * Validation on save: Saving a clock runs a dry check of the template (available tracks per playlist, element durations against tolerances, total fit into 60 minutes, placement of fixed elements). Problems are shown immediately, not at air time.
  * Overflow: If a track runs past the end of its segment (a fixed element or the end of the hour), that track is kept and fades out over 2 to 3 seconds (default 2.5 s), ending at the segment end. The tracks after it in that segment are removed from the clock. This keeps the air filled without leaving a gap. Overflow does not trigger filler. Filler is used only when the clock is short.
    * A truncated track counts as aired. It is logged with its actual played duration and counts toward separation rules like any other track.
    * Removed tracks are not aired and are not missed. They do not count toward separation and go back to their playlist.
  * Crossing the hour: The hour boundary is hard. Each clock ends exactly at the end of its hour, and the next clock starts exactly on time. A track that starts near the end of a clock is cut at the boundary and does not carry over.
  * Artist/Track Separation: Each track must repeat no more than once every three days. Other separation rules are stored in the separation_rules table; their values are copied from the reference software. Only items that actually aired count toward separation. Items marked as missed do not.
  * Grid changes: Changing a clock or the grid recalculates only planned, not yet aired items in the affected hours. Open reminders for those hours are expired.
* Broadcast Failure Handling: If the broadcast is interrupted (for example, the Icecast server goes down, or the internet connection is lost), the system must record which scheduled items did not reach the air.
  * Interruptions are detected automatically and stored as outage periods with a start time, an end time, and the component that failed (for example, Icecast, the Liquidsoap control link, or the backend).
  * Items that were planned for an outage period and did not air are marked as "missed". Missed items are stored with their planned start time, planned duration, the clock and hour they belonged to, and the reason for the interruption.
  * The played-tracks list in the admin panel shows aired, truncated, and missed items together. Missed items are marked clearly (for example, a "Not aired" label), so the manager can see at a glance which programs or segments were not broadcast.
  * Missed items are kept until the manager resolves them. They are not removed by the normal retention period.
  * The manager can reschedule a missed item into a following hour. The manager chooses the placement (start, end, or a fixed minute), and the hour is rebuilt around it by the same rules as any other element. This is a one-time change for that hour; the clock template is not changed. If the item does not fit, the request is refused.
  * The manager can also dismiss a missed item if it should not be broadcast again.
  * If the outage ends while the backend is down, the system reconciles what actually aired from the playout log kept by Liquidsoap, so the played-tracks list is accurate after recovery.
* Grid Management: Ability to create various clock templates and assign them to specific hours, days, and timeslots, up to 30 days ahead.
* Play Rules: A separate page where the manager creates named, reusable play rules and applies them to tracks (mainly jingles) or to whole playlists. This replaces dayparting.
  * A rule may combine: a time window of the day with weekdays (for example, 06:00 to 08:00), a minimum interval between plays of the same track (for example, 10 minutes), and a maximum number of plays per hour (for example, 1).
  * Examples: one jingle plays only from 06:00 to 08:00 and another only from 10:00 to 12:00; a jingle plays at most once per hour; a jingle plays no more often than every 10 minutes.
  * A rule limits when a track may play. How often a slot appears in the hour (for example, a jingle every 15 minutes) is set by the clock structure.
  * A track can have several rules, and all of them must pass. A track also follows the rules of its playlists.
  * Rules are checked at planning time, including for filler and rescheduled items. A rule is never relaxed. If no track passes, the element is skipped and the operator is warned.
* Audio Quality Analysis: The system should automatically read a track's audio quality and format, providing a warning or recommendation if the quality falls below the required threshold.
* Audio Transcoding: Built-in tools to reformat/transcode audio files to match the required broadcast standard.
