# Minimal development playout script for Liquidsoap 2.4.x.
#
# Uses the same mechanisms as production (§8 of docs/tech-design-7-8.md):
#   - a request queue fed by the backend over telnet (`request.push <uri>`);
#   - a safety source behind `fallback`, so the stream never stops;
#   - a harbor input for live voice and an interactive `music_gain` variable;
#   - output to Icecast;
#   - a UTC playout journal and an HTTP callback on every track start and end.
#
# Every startup value comes from the environment (set in docker-compose.dev.yml).
# The production script lives in radio-atlantida-deploy/config/. When the
# interface the backend relies on changes (telnet commands, callback payload,
# journal format), both scripts are updated in the same release.

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

def report(event, m, elapsed_ms) =
  line =
    json.stringify(
      compact=true,
      {
        event=event,
        time=utc_now(),
        rid=m["rid"],
        uri=m["initial_uri"],
        filename=m["filename"],
        elapsed_ms=elapsed_ms
      }
    )
  journal(line)
  callback(line)
end

# --- Sources -------------------------------------------------------------

# Request queue fed by the backend: `request.push <uri>` over telnet.
queue = request.queue(id="request")

# Track start and end of queued items only. The safety source is not an
# automation item and is not reported.
queue.on_track(synchronous=true, fun (m) -> report("start", m, 0))
queue.on_position(
  synchronous=true,
  position={0.},
  remaining=true,
  allow_partial=true,
  fun (_, m) -> report("end", m, int(queue.elapsed() * 1000.))
)

# Runtime music gain, changed by the backend: `var.set music_gain = 0.2`.
music_gain = interactive.float("music_gain", min=0., max=1., 1.)
music = amplify(music_gain, queue)

# Safety source: a quiet sine tone, so the stream never stops (§8.6).
safety = amplify(0.1, sine(id="safety", 440.))
automation = fallback(id="automation", track_sensitive=false, [music, safety])

# Live voice from the backend WebRTC bridge, mixed over automation (§8.4).
live =
  input.harbor(
    id="live",
    port=harbor_port,
    user=harbor_user,
    password=harbor_password,
    "live"
  )

radio = add(id="mix", normalize=false, [live, automation])

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
