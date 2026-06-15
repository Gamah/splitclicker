# data/ — host-editable runtime config

Everything here is read off disk by the server **on every request**, so changes
apply live — no rebuild, no container restart. In the Docker stack this whole
directory is bind-mounted into the container at `/data` (see
`docker/docker-compose.yml`).

## Set the winning skin / countdown

1. Copy the example once: `cp config.json.example config.json`
2. Edit `config.json`:
   ```json
   {
     "skin_image": "skin2win.png",
     "winner_lock_time": "2026-06-16T07:00:00Z"
   }
   ```
   - `skin_image` — a filename inside `media/` (served at `GET /api/v1/skin`).
   - `winner_lock_time` — RFC3339; the client counts down to it. Leave empty/omit
     to hide the countdown. Once it passes, the HUD prompts for a new skin.

`config.json` is gitignored (it's the live, host-owned file); `config.json.example`
is the committed template. If `config.json` is missing or a field is blank, the
server falls back to the `SKIN_IMAGE` / `WINNER_LOCK_TIME` env vars, then defaults.

## Change the skin image bytes

Drop a new file into `media/` and point `skin_image` at it (or overwrite the
existing file). Served immediately.
