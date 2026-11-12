# radio-atlantida-contract: 1
# min-backend: 1.0.0
#
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
#   - output to Icecast from the backend's stream settings, replaced at
#     runtime by `output.set <json>` (§8.8): read at startup from
#     BACKEND_OUTPUT_URL, else from OUTPUT_STATE_FILE (the last output that
#     connected), else from the environment; plain or TLS; its connection
#     state reported to the backend;
#   - a UTC playout journal and an HTTP callback on every track start and end;
#   - a heartbeat callback every 5 seconds, for the outage detector.
#
# Every startup value comes from the environment (set in docker-compose.dev.yml).
# The production script lives in radio-atlantida-deploy/config/ and is this
# file plus a production patch.
#
# The contract (first lines): the interface the backend relies on is the
# telnet commands (hour_<p>.push/.queue/.flush_and_skip, var.set, output.set),
# the callback payload, the journal format, and the emergency and output
# endpoints. Whenever one of them changes, raise radio-atlantida-contract
# by one and set min-backend to the first backend release that implements
# the new contract. The deploy tool refuses a backend below min-backend and
# restarts Liquidsoap (maintenance) when the contract number changes.

def env(name, default) =
  environment.get(default=default, name)
end

icecast_host = env("ICECAST_HOST", "icecast")
icecast_port = int_of_string(env("ICECAST_PORT", "8000"))
icecast_mount = env("ICECAST_MOUNT", "/radio")
icecast_tls = env("ICECAST_TLS", "false") == "true"
icecast_password = env("ICECAST_SOURCE_PASSWORD", "hackme")
telnet_port = int_of_string(env("TELNET_PORT", "1234"))
harbor_port = int_of_string(env("HARBOR_PORT", "8005"))
harbor_user = env("LIQUIDSOAP_CONTROL_USER", "source")
harbor_password = env("LIQUIDSOAP_CONTROL_PASSWORD", "hackme")
journal_dir = env("JOURNAL_DIR", "/journal")
callback_url =
  env("BACKEND_CALLBACK_URL", "http://host.docker.internal:8090/api/playout/callback")
emergency_url =
  env(
    "BACKEND_EMERGENCY_URL",
    "http://host.docker.internal:8090/api/playout/emergency/next"
  )
output_url =
  env("BACKEND_OUTPUT_URL", "http://host.docker.internal:8090/api/playout/output")
# The image's own cache directory: it belongs to the user liquidsoap the
# container runs as, so a named volume mounted over it is writable (§8.8).
output_state_file =
  env("OUTPUT_STATE_FILE", "/var/cache/liquidsoap/output.json")

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
# URI is annotated with item_id; the track's cue points as liq_cue_in and
# liq_cue_out (applied when the request is resolved); its fades as
# liq_fade_in and liq_fade_out, or for a truncated item liq_cue_out and the
# fade that ends there (applied by fade.in and fade.out below).
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

# A track with liq_fade_in fades in over that many seconds from its cue-in
# point; one with liq_fade_out (a truncated item, or a track's own fade
# out) fades out over that many seconds, ending at its cue-out point. Other
# tracks are not faded (docs/phase-22-plan.md).
#
# fade.out must be track_sensitive: in Liquidsoap 2.4 the default (false)
# starts one fade on the first frame of the source and never resets it, so
# the queues stay silent after the first fade. fade.in is not used: it reads
# its override in on_metadata but starts in on_track, and for a track that
# starts after the queue was empty the metadata comes second, so the fade
# is lost. cue_fade_in takes the duration from the metadata on_track gives.
def cue_fade_in(s) =
  fade_length = ref(0.)
  fade_start = ref(0.)
  s.on_track(
    synchronous=true,
    fun (m) ->
      begin
        fade_length := float_of_string(default=0., m["liq_fade_in"])
        fade_start := source.time(s)
      end
  )
  amplify(
    override=null,
    fun () ->
      if
        fade_length() <= 0.
      then
        1.
      else
        min(1., max(0., (source.time(s) - fade_start()) / fade_length()))
      end,
    s
  )
end

hours = cue_fade_in(hours)
hours =
  fade.out(
    duration=0., override_duration="liq_fade_out", track_sensitive=true, hours
  )

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

# Emergency tracks carry the cue points and fades of their track, like plan
# items.
emergency = cue_fade_in(emergency)
emergency =
  fade.out(
    duration=0.,
    override_duration="liq_fade_out",
    track_sensitive=true,
    emergency
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

# A heartbeat every 5 seconds, callback only (not journaled): the outage
# detector tells a stalled Liquidsoap from a long track by it (§7.1). It also
# says whether the safety tone is on air, that is, whether the source before
# it in the fallback (the hour queues and the emergency source) has nothing
# to play. Below `music`, because it reads it.
thread.run(
  every=5.,
  fun () ->
    callback(
      json.stringify(
        compact=true,
        {
          event="heartbeat",
          time=utc_now(),
          safety_on_air=not source.is_ready(music)
        }
      )
    )
)

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

# --- Stream output (§8.8) -----------------------------------------------

# The Icecast output is a backend setting group (stream.*) applied at
# runtime: `output.set <json>` replaces the output on the same `radio`
# source, so the queues, the fallback, and the journal go on (a reconnect,
# not a restart, §6.3). The JSON object is the body of
# GET /api/playout/output: host, port, mount, tls, codec, bitrate_kbps,
# sample_rate_hz, channels, name, description, genre. Every field is
# required; unknown fields are ignored. It is parsed with json.parse, so no
# value ever becomes script code; the source password stays in this
# environment and is never sent.
def parse_output(s) =
  let json.parse (
    c :
      {
        host: string,
        port: int,
        mount: string,
        tls: bool,
        codec: string,
        bitrate_kbps: int,
        sample_rate_hz: int,
        channels: int,
        name: string,
        description: string,
        genre: string
      }
  ) = s
  c
end

# The canonical form of an output: equal outputs give equal strings.
def output_json(c) =
  json.stringify(
    compact=true,
    {
      host=c.host,
      port=c.port,
      mount=c.mount,
      tls=c.tls,
      codec=c.codec,
      bitrate_kbps=c.bitrate_kbps,
      sample_rate_hz=c.sample_rate_hz,
      channels=c.channels,
      name=c.name,
      description=c.description,
      genre=c.genre
    }
  )
end

# ENCODER_LIMITS: the highest bitrate (kbps) the encoders of Liquidsoap
# 2.4.6 accept per codec, channel count, and sample rate, where it is below
# 320. Found by running every combination of the settings (2026-11-12):
# above it the encoder raises when the output starts and Liquidsoap exits.
# The same table is internal/streamformat of the backend (the settings
# check refuses these at save; a unit test in dev/liqsmoke compares the
# two); dev/liqsmoke checks both sides of every limit.
encoder_limits =
  [
    ("vorbis", 1, 22050, 64),
    ("vorbis", 1, 32000, 160),
    ("vorbis", 1, 44100, 192),
    ("vorbis", 1, 48000, 192),
    ("vorbis", 2, 22050, 160)
  ]

def encoder_error(c) =
  limits =
    list.filter(
      fun (l) ->
        begin
          let (codec, channels, rate, _) = l
          codec == c.codec and channels == c.channels and rate == c.sample_rate_hz
        end,
      encoder_limits
    )
  if
    limits == []
  then
    ""
  else
    let (_, _, _, max_kbps) = list.hd(limits)
    if
      c.bitrate_kbps > max_kbps
    then
      "#{c.codec} with #{c.channels} channel(s) at #{c.sample_rate_hz} Hz allows at most #{max_kbps} kbps"
    else
      ""
    end
  end
end

# The enums are those of the settings (internal/settings/definitions.go).
# Some combinations are refused by the encoders at runtime, which
# `liquidsoap --check` does not see; they are refused here, so the running
# output stays (ENCODER_LIMITS).
def output_error(c) =
  if
    c.host == ""
  then
    "host must not be empty"
  elsif
    c.port < 1 or c.port > 65535
  then
    "port must be between 1 and 65535"
  elsif
    string.length(c.mount) < 2 or string.sub(c.mount, start=0, length=1) != "/"
  then
    "mount must start with /"
  elsif
    not list.mem(c.codec, ["mp3", "aac", "vorbis"])
  then
    "codec must be mp3, aac or vorbis"
  elsif
    not list.mem(c.bitrate_kbps, [64, 96, 128, 160, 192, 256, 320])
  then
    "bitrate_kbps must be 64, 96, 128, 160, 192, 256 or 320"
  elsif
    not list.mem(c.sample_rate_hz, [22050, 32000, 44100, 48000])
  then
    "sample_rate_hz must be 22050, 32000, 44100 or 48000"
  elsif
    c.channels != 1 and c.channels != 2
  then
    "channels must be 1 or 2"
  else
    encoder_error(c)
  end
end

# The last output that connected, for a start without the backend. Saved
# on connect, not on output.set: an output that cannot start (an encoder
# the checks above missed) is never a startup output. A missing directory
# means no state file, not an error.
def save_output(key) =
  try
    file.write(
      atomic=true, temp_dir=path.dirname(output_state_file), data=key, output_state_file
    )
  catch err do
    log.info(label="output", "output state not saved: #{err.message}")
  end
end

# A new output.icecast on `radio` (mono: its mean). One branch per codec
# and channel count: %mp3 takes `stereo` only as a literal (a runtime value
# passes --check and fails when the output starts). Outputs are created
# with register_telnet=false: their ids would repeat on every switch.
def make_output(c) =
  br = c.bitrate_kbps
  sr = c.sample_rate_hz
  # TLS with http.transport.tls(), which verifies the server against the
  # system roots. http.transport.ssl() of the 2.4.6 image rejects public
  # chains (Let's Encrypt included: "unable to get local issuer
  # certificate"), checked 2026-11-12.
  transport = if c.tls then http.transport.tls() else http.transport.unix end
  mono = mean(radio)
  def icecast(enc, s) =
    output.icecast(
      id="icecast",
      register_telnet=false,
      enc,
      host=c.host,
      port=c.port,
      password=icecast_password,
      mount=c.mount,
      name=c.name,
      description=c.description,
      genre=c.genre,
      transport=transport,
      s
    )
  end
  o =
    if
      c.codec == "mp3" and c.channels == 2
    then
      icecast(%mp3(bitrate = br, samplerate = sr, stereo = true), radio)
    elsif
      c.codec == "mp3"
    then
      icecast(%mp3(bitrate = br, samplerate = sr, stereo = false), mono)
    elsif
      c.codec == "vorbis" and c.channels == 2
    then
      icecast(%vorbis.cbr(bitrate = br, samplerate = sr, channels = 2), radio)
    elsif
      c.codec == "vorbis"
    then
      icecast(%vorbis.cbr(bitrate = br, samplerate = sr, channels = 1), mono)
    elsif
      c.channels == 2
    then
      icecast(
        %fdkaac(aot = "mpeg4_aac_lc", bitrate = br, samplerate = sr, channels = 2),
        radio
      )
    else
      icecast(
        %fdkaac(aot = "mpeg4_aac_lc", bitrate = br, samplerate = sr, channels = 1),
        mono
      )
    end
  # Retry after 1 s, not the default 3 s: after a switch on the same mount
  # Icecast refuses the new source (403) until the old one has gone.
  o.on_error(synchronous=false, fun (~restart_in, _) -> restart_in(1.))
  key = output_json(c)
  o.on_connect(
    synchronous=false,
    fun () ->
      begin
        report_output(true)
        save_output(key)
      end
  )
  o.on_disconnect(synchronous=false, fun () -> report_output(false))
  o
end

def checked_output(origin, s) =
  try
    c = parse_output(s)
    e = output_error(c)
    if
      e == ""
    then
      c
    else
      log.important(label="output", "output from #{origin} refused: #{e}")
      null
    end
  catch err do
    log.important(label="output", "output from #{origin} unreadable: #{err.message}")
    null
  end
end

# Startup: the backend, then the last applied output, then the environment
# with the settings defaults. The backend sends output.set as soon as its
# control link connects, which corrects any of them.
def backend_output() =
  try
    resp = http.get(timeout=2., output_url)
    if
      resp.status_code == 200
    then
      checked_output("the backend", resp)
    else
      log.important(label="output", "output from the backend: HTTP #{resp.status_code}")
      null
    end
  catch err do
    log.important(label="output", "output from the backend failed: #{err.message}")
    null
  end
end

def state_output() =
  if
    file.exists(output_state_file)
  then
    checked_output("the state file", file.contents(output_state_file))
  else
    null
  end
end

env_output =
  {
    host=icecast_host,
    port=icecast_port,
    mount=icecast_mount,
    tls=icecast_tls,
    codec="mp3",
    bitrate_kbps=128,
    sample_rate_hz=44100,
    channels=2,
    name="Radio Atlantida",
    description="",
    genre=""
  }

initial_output =
  begin
    b = backend_output()
    if
      null.defined(b)
    then
      log.important(label="output", "startup output from the backend")
      null.get(b)
    else
      st = state_output()
      if
        null.defined(st)
      then
        log.important(label="output", "startup output from the state file")
        null.get(st)
      else
        log.important(label="output", "startup output from the environment")
        env_output
      end
    end
  end

current_output = ref(make_output(initial_output))
current_cfg = ref(output_json(initial_output))
log.important(label="output", "output: #{current_cfg()}")

# output.set <json>: "OK unchanged" when it equals the running output (so
# the backend may send it on every connect), "OK applied" after the switch,
# "ERROR <reason>" when refused (the running output stays). The old output
# is shut down first: Icecast takes the mount only after it has gone.
def set_output(arg) =
  try
    c = parse_output(arg)
    e = output_error(c)
    if
      e != ""
    then
      "ERROR #{e}"
    else
      key = output_json(c)
      if
        key == current_cfg()
      then
        "OK unchanged"
      else
        current_output().shutdown()
        current_output := make_output(c)
        current_cfg := key
        log.important(label="output", "output applied: #{key}")
        "OK applied"
      end
    end
  catch err do
    "ERROR invalid output: #{err.message}"
  end
end
server.register(
  usage="output.set <json>",
  description="Replace the Icecast output (JSON as GET /api/playout/output).",
  "output.set",
  set_output
)
