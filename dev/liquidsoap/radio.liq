# Minimal development playout script for Liquidsoap 2.4.x.
#
# Uses the same mechanisms as production (§8 of docs/tech-design.md and
# docs/phase-9-plan.md §2):
#   - two request queues, hour_0 and hour_1, fed by the backend over telnet
#     (`hour_<p>.push <uri>`); a time switch plays the queue of the current
#     station hour, so every clock starts exactly on its boundary;
#   - the queue an hour leaves is flushed, so its leftovers never play later;
#   - an emergency source (request.dynamic, asking the backend) and a safety
#     source behind `fallback`, so the stream never stops;
#   - a harbor input for live voice, fed by the backend's WebRTC bridge and
#     mixed over the music, which ducks to `duck_gain` while it is live; an
#     interactive `music_gain` variable;
#   - output to Icecast, with its connection state reported to the backend;
#   - a UTC playout journal and an HTTP callback on every track start and end;
#   - a heartbeat callback every 5 seconds, for the outage detector.
#
# Every startup value comes from the environment (set in docker-compose.dev.yml).
# The production script lives in radio-atlantida-deploy/config/. When the
# interface the backend relies on changes (telnet commands, callback payload,
# journal format, emergency endpoint), both scripts are updated in the same
# release.

def env(name, default) =
  environment.get(default=default, name)
end

icecast_host = env("ICECAST_HOST", "icecast")
icecast_port = int_of_string(env("ICECAST_PORT", "8000"))
icecast_mount = env("ICECAST_MOUNT", "/radio")
icecast_password = env("ICECAST_SOURCE_PASSWORD", "hackme")
telnet_port = int_of_string(env("TELNET_PORT", "1234"))
harbor_port = int_of_string(env("HARBOR_PORT", "8005"))
harbor_user = env("LIQUIDSOAP_CONTROL_USER", "source")
harbor_password = env("LIQUIDSOAP_CONTROL_PASSWORD", "hackme")
journal_dir = env("JOURNAL_DIR", "/journal")
callback_url =
  env("BACKEND_CALLBACK_URL", "http://host.docker.internal:8080/api/playout/callback")
emergency_url =
  env(
    "BACKEND_EMERGENCY_URL",
    "http://host.docker.internal:8080/api/playout/emergency/next"
  )

settings.log.stdout := true
settings.log.file := false
settings.log.level := 3

# The control interface is reachable only on the private Docker network (and on
# 127.0.0.1 of the dev host through the compose port binding, §8.9).
settings.server.telnet := true
settings.server.telnet.bind_addr := "0.0.0.0"
settings.server.telnet.port := telnet_port
settings.harbor.bind_addrs := ["0.0.0.0"]

# --- Playout journal and callbacks ---------------------------------------

# RFC 3339 UTC timestamp with milliseconds. The container runs with TZ=UTC.
def utc_now() =
  t = time()
  ms = int((t - floor(t)) * 1000.)
  pad = if ms < 10 then "00" elsif ms < 100 then "0" else "" end
  time.string(time=t, "%Y-%m-%dT%H:%M:%S") ^ "." ^ pad ^ string(ms) ^ "Z"
end

# One JSON line per event, appended to journal-YYYY-MM-DD.jsonl (UTC date).
# The journal is written independently of the backend, so it survives a
# backend outage and is used for reconciliation (§7.1).
def journal(line) =
  journal_path = journal_dir ^ "/journal-" ^ time.string("%Y-%m-%d") ^ ".jsonl"
  try
    file.write(append=true, data=line ^ "\n", journal_path)
  catch err do
    log.important(label="journal", "journal write failed: #{err}")
  end
end

# The callback runs in its own thread: a slow or missing backend never delays
# playout, and a failed callback is ignored (the journal still has the event).
def callback(line) =
  thread.run(
    fast=false,
    fun () ->
      try
        ignore(
          http.post(
            timeout=2.,
            headers=[("Content-Type", "application/json")],
            data=line,
            callback_url
          )
        )
      catch err do
        log.info(label="callback", "callback failed: #{err}")
      end
  )
end

# A track event of a plan item (source "plan", with the item_id the backend
# annotated) or of an emergency track (source "emergency", with track_id).
def report(event, source, m, elapsed_ms) =
  line =
    json.stringify(
      compact=true,
      {
        event=event,
        time=utc_now(),
        source=source,
        rid=m["rid"],
        item_id=m["item_id"],
        track_id=m["track_id"],
        uri=m["initial_uri"],
        filename=m["filename"],
        elapsed_ms=elapsed_ms
      }
    )
  journal(line)
  callback(line)
end

# A heartbeat every 5 seconds, callback only (not journaled): the outage
# detector tells a stalled Liquidsoap from a long track by it (§7.1).
thread.run(
  every=5.,
  fun () ->
    callback(
      json.stringify(compact=true, {event="heartbeat", time=utc_now()})
    )
)

# The journal is kept 10 days (§7.1). The backend mounts it read-only, so
# Liquidsoap deletes the older files, checking a few days back in case it was
# not running when a file came of age.
thread.run(
  every=3600.,
  fun () ->
    list.iter(
      fun (d) ->
        begin
          name =
            journal_dir ^ "/journal-" ^
              time.string(time=time() - float(d) * 86400., "%Y-%m-%d") ^
              ".jsonl"
          if file.exists(name) then file.remove(name) end
        end,
      [11, 12, 13, 14, 15]
    )
)

# The Icecast output connection state (§8.3), for the outage detector.
def report_output(connected) =
  line =
    json.stringify(
      compact=true, {event="output", time=utc_now(), connected=connected}
    )
  journal(line)
  callback(line)
end

# Reports start and end of every track of s.
def reported(source_name, s) =
  s.on_track(synchronous=true, fun (m) -> report("start", source_name, m, 0))
  s.on_position(
    synchronous=true,
    position={0.},
    remaining=true,
    allow_partial=true,
    fun (_, m) -> report("end", source_name, m, int(s.elapsed() * 1000.))
  )
  s
end

# --- Sources -------------------------------------------------------------

# The station hour is fed into hour_<parity>. hour_shift is the station UTC
# offset modulo one hour, in seconds (0 for whole-hour offsets such as
# Asia/Yerevan); the backend sets it on connect with
# `var.set hour_shift = <seconds>` and uses the same formula to choose the
# queue (internal/playout).
hour_shift = interactive.float("hour_shift", min=0., max=3599., 0.)

def hour_parity() =
  int(floor((time() - hour_shift()) / 3600.)) mod 2
end

# Request queues fed by the backend: `hour_<p>.push <uri>` over telnet. The
# URI is annotated with item_id and, for a truncated item, liq_cue_out and
# liq_fade_out (applied when the request is resolved, and by fade.out below).
hour_0 = reported("plan", request.queue(id="hour_0"))
hour_1 = reported("plan", request.queue(id="hour_1"))

# When the hour changes, the queue of the hour that ended is flushed: its
# leftovers (drift, a late push) must not play in the next hour of the same
# parity. A track cut by the switch reports its end now; the queue reports it
# again when it is selected an hour later, and the backend keeps the first.
# The switch itself is checked on every frame.
def end_hour(q) =
  if null.defined(q.current()) then
    report("end", "plan", q.last_metadata() ?? [], int(q.elapsed() * 1000.))
  end
  ignore(server.execute("#{q.id()}.flush_and_skip"))
end

last_parity = ref(hour_parity())
thread.run(
  every=0.1,
  fun () ->
    begin
      p = hour_parity()
      if p != last_parity() then
        if last_parity() == 0 then end_hour(hour_0) else end_hour(hour_1) end
        last_parity := p
      end
    end
)

hours =
  switch(
    id="hours",
    track_sensitive=false,
    [({hour_parity() == 0}, hour_0), ({hour_parity() == 1}, hour_1)]
  )

# A truncated item fades out over liq_fade_out seconds, ending at its cue-out
# point; other tracks are not faded.
hours = fade.out(duration=0., override_duration="liq_fade_out", hours)

# Emergency (§8.2, §8.6): when the hour queue has nothing to play, ask the
# backend for a track of the emergency playlist. 204 or an error: nothing,
# and the safety source plays.
def next_emergency() =
  try
    resp = http.get(timeout=2., emergency_url)
    if resp.status_code == 200 and string.trim(resp) != "" then
      request.create(string.trim(resp))
    else
      null
    end
  catch err do
    log.info(label="emergency", "emergency request failed: #{err}")
    null
  end
end
emergency =
  reported(
    "emergency",
    request.dynamic(id="emergency", retry_delay={5.}, next_emergency)
  )

# Runtime music gain, changed by the backend: `var.set music_gain = 0.2`.
music_gain = interactive.float("music_gain", min=0., max=1., 1.)
music =
  amplify(
    music_gain,
    fallback(id="automation_main", track_sensitive=false, [hours, emergency])
  )

# Safety source: a quiet sine tone, so the stream never stops (§8.6).
safety = amplify(0.1, sine(id="safety", 440.))
automation = fallback(id="automation", track_sensitive=false, [music, safety])

# Live voice from the backend WebRTC bridge (§8.4): an Ogg/Opus stream sent
# with the Icecast source protocol to the mount `live` (fixed: the backend
# uses the same name). The buffer is short, so voice reaches air within the
# 1 s target; the default (12 s) is meant for crossfades of a remote DJ.
live =
  input.harbor(
    id="live",
    port=harbor_port,
    user=harbor_user,
    password=harbor_password,
    buffer=0.3,
    max=2.,
    "live"
  )

# Ducking (§8.5): while the live input has a source, automation fades to
# duck_gain of its level, and back when it ends. The backend sets the value
# from the setting liquidsoap.duck_gain: `var.set duck_gain = 0.2`.
duck_gain = interactive.float("duck_gain", min=0., max=1., 0.2)
radio = smooth_add(duration=0.5, p=duck_gain, normal=automation, special=live)

out =
  output.icecast(
    id="icecast",
    %mp3(bitrate = 128, samplerate = 44100, stereo = true),
    host=icecast_host,
    port=icecast_port,
    password=icecast_password,
    mount=icecast_mount,
    name="Radio Atlantida (dev)",
    radio
  )
out.on_connect(synchronous=false, fun () -> report_output(true))
out.on_disconnect(synchronous=false, fun () -> report_output(false))
